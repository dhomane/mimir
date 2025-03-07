// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/cortexproject/cortex/blob/master/pkg/querier/blocks_store_queryable.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Cortex Authors.

package querier

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/gogo/protobuf/types"
	"github.com/grafana/dskit/kv"
	"github.com/grafana/dskit/ring"
	"github.com/grafana/dskit/services"
	"github.com/oklog/ulid"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/thanos-io/thanos/pkg/block"
	"github.com/thanos-io/thanos/pkg/extprom"
	"github.com/thanos-io/thanos/pkg/objstore"
	"github.com/thanos-io/thanos/pkg/store/hintspb"
	"github.com/thanos-io/thanos/pkg/store/storepb"
	"github.com/thanos-io/thanos/pkg/strutil"
	"go.uber.org/atomic"
	"golang.org/x/sync/errgroup"
	grpc_metadata "google.golang.org/grpc/metadata"

	"github.com/grafana/dskit/tenant"

	"github.com/grafana/mimir/pkg/mimirpb"
	"github.com/grafana/mimir/pkg/querier/stats"
	"github.com/grafana/mimir/pkg/storage/bucket"
	"github.com/grafana/mimir/pkg/storage/series"
	"github.com/grafana/mimir/pkg/storage/sharding"
	mimir_tsdb "github.com/grafana/mimir/pkg/storage/tsdb"
	"github.com/grafana/mimir/pkg/storage/tsdb/bucketindex"
	"github.com/grafana/mimir/pkg/storegateway"
	"github.com/grafana/mimir/pkg/storegateway/storegatewaypb"
	"github.com/grafana/mimir/pkg/util"
	"github.com/grafana/mimir/pkg/util/globalerror"
	"github.com/grafana/mimir/pkg/util/limiter"
	util_log "github.com/grafana/mimir/pkg/util/log"
	"github.com/grafana/mimir/pkg/util/math"
	"github.com/grafana/mimir/pkg/util/spanlogger"
	"github.com/grafana/mimir/pkg/util/validation"
)

const (
	// The maximum number of times we attempt fetching missing blocks from different
	// store-gateways. If no more store-gateways are left (ie. due to lower replication
	// factor) than we'll end the retries earlier.
	maxFetchSeriesAttempts = 3
)

var (
	maxChunksPerQueryLimitMsgFormat = globalerror.MaxChunksPerQuery.MessageWithPerTenantLimitConfig(
		"the query exceeded the maximum number of chunks fetched from store-gateways when querying '%s' (limit: %d)",
		validation.MaxChunksPerQueryFlag,
	)
)

// BlocksStoreSet is the interface used to get the clients to query series on a set of blocks.
type BlocksStoreSet interface {
	services.Service

	// GetClientsFor returns the store gateway clients that should be used to
	// query the set of blocks in input. The exclude parameter is the map of
	// blocks -> store-gateway addresses that should be excluded.
	GetClientsFor(userID string, blockIDs []ulid.ULID, exclude map[ulid.ULID][]string) (map[BlocksStoreClient][]ulid.ULID, error)
}

// BlocksFinder is the interface used to find blocks for a given user and time range.
type BlocksFinder interface {
	services.Service

	// GetBlocks returns known blocks for userID containing samples within the range minT
	// and maxT (milliseconds, both included). Returned blocks are sorted by MaxTime descending.
	GetBlocks(ctx context.Context, userID string, minT, maxT int64) (bucketindex.Blocks, map[ulid.ULID]*bucketindex.BlockDeletionMark, error)
}

// BlocksStoreClient is the interface that should be implemented by any client used
// to query a backend store-gateway.
type BlocksStoreClient interface {
	storegatewaypb.StoreGatewayClient

	// RemoteAddress returns the address of the remote store-gateway and is used to uniquely
	// identify a store-gateway backend instance.
	RemoteAddress() string
}

// BlocksStoreLimits is the interface that should be implemented by the limits provider.
type BlocksStoreLimits interface {
	bucket.TenantConfigProvider

	MaxLabelsQueryLength(userID string) time.Duration
	MaxChunksPerQuery(userID string) int
	StoreGatewayTenantShardSize(userID string) int
}

type blocksStoreQueryableMetrics struct {
	storesHit prometheus.Histogram
	refetches prometheus.Histogram

	blocksFound                                       prometheus.Counter
	blocksQueried                                     prometheus.Counter
	blocksWithCompactorShardButIncompatibleQueryShard prometheus.Counter
}

func newBlocksStoreQueryableMetrics(reg prometheus.Registerer) *blocksStoreQueryableMetrics {
	return &blocksStoreQueryableMetrics{
		storesHit: promauto.With(reg).NewHistogram(prometheus.HistogramOpts{
			Namespace: "cortex",
			Name:      "querier_storegateway_instances_hit_per_query",
			Help:      "Number of store-gateway instances hit for a single query.",
			Buckets:   []float64{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
		}),
		refetches: promauto.With(reg).NewHistogram(prometheus.HistogramOpts{
			Namespace: "cortex",
			Name:      "querier_storegateway_refetches_per_query",
			Help:      "Number of re-fetches attempted while querying store-gateway instances due to missing blocks.",
			Buckets:   []float64{0, 1, 2},
		}),

		blocksFound: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "cortex_querier_blocks_found_total",
			Help: "Number of blocks found based on query time range.",
		}),
		blocksQueried: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "cortex_querier_blocks_queried_total",
			Help: "Number of blocks queried to satisfy query. Compared to blocks found, some blocks may have been filtered out thanks to query and compactor sharding.",
		}),
		blocksWithCompactorShardButIncompatibleQueryShard: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "cortex_querier_blocks_with_compactor_shard_but_incompatible_query_shard_total",
			Help: "Blocks that couldn't be checked for query and compactor sharding optimization due to incompatible shard counts.",
		}),
	}
}

// BlocksStoreQueryable is a queryable which queries blocks storage via
// the store-gateway.
type BlocksStoreQueryable struct {
	services.Service

	stores          BlocksStoreSet
	finder          BlocksFinder
	consistency     *BlocksConsistencyChecker
	logger          log.Logger
	queryStoreAfter time.Duration
	metrics         *blocksStoreQueryableMetrics
	limits          BlocksStoreLimits

	// Subservices manager.
	subservices        *services.Manager
	subservicesWatcher *services.FailureWatcher
}

func NewBlocksStoreQueryable(
	stores BlocksStoreSet,
	finder BlocksFinder,
	consistency *BlocksConsistencyChecker,
	limits BlocksStoreLimits,
	queryStoreAfter time.Duration,
	logger log.Logger,
	reg prometheus.Registerer,
) (*BlocksStoreQueryable, error) {
	manager, err := services.NewManager(stores, finder)
	if err != nil {
		return nil, errors.Wrap(err, "register blocks storage queryable subservices")
	}

	q := &BlocksStoreQueryable{
		stores:             stores,
		finder:             finder,
		consistency:        consistency,
		queryStoreAfter:    queryStoreAfter,
		logger:             logger,
		subservices:        manager,
		subservicesWatcher: services.NewFailureWatcher(),
		metrics:            newBlocksStoreQueryableMetrics(reg),
		limits:             limits,
	}

	q.Service = services.NewBasicService(q.starting, q.running, q.stopping)

	return q, nil
}

func NewBlocksStoreQueryableFromConfig(querierCfg Config, gatewayCfg storegateway.Config, storageCfg mimir_tsdb.BlocksStorageConfig, limits BlocksStoreLimits, logger log.Logger, reg prometheus.Registerer) (*BlocksStoreQueryable, error) {
	var (
		stores       BlocksStoreSet
		bucketClient objstore.Bucket
	)

	bucketClient, err := bucket.NewClient(context.Background(), storageCfg.Bucket, "querier", logger, reg)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create bucket client")
	}

	// Blocks finder doesn't use chunks, but we pass config for consistency.
	cachingBucket, err := mimir_tsdb.CreateCachingBucket(storageCfg.BucketStore.ChunksCache, storageCfg.BucketStore.MetadataCache, bucketClient, logger, extprom.WrapRegistererWith(prometheus.Labels{"component": "querier"}, reg))
	if err != nil {
		return nil, errors.Wrap(err, "create caching bucket")
	}
	bucketClient = cachingBucket

	// Create the blocks finder.
	var finder BlocksFinder
	if storageCfg.BucketStore.BucketIndex.Enabled {
		finder = NewBucketIndexBlocksFinder(BucketIndexBlocksFinderConfig{
			IndexLoader: bucketindex.LoaderConfig{
				CheckInterval:         time.Minute,
				UpdateOnStaleInterval: storageCfg.BucketStore.SyncInterval,
				UpdateOnErrorInterval: storageCfg.BucketStore.BucketIndex.UpdateOnErrorInterval,
				IdleTimeout:           storageCfg.BucketStore.BucketIndex.IdleTimeout,
			},
			MaxStalePeriod:           storageCfg.BucketStore.BucketIndex.MaxStalePeriod,
			IgnoreDeletionMarksDelay: storageCfg.BucketStore.IgnoreDeletionMarksDelay,
		}, bucketClient, limits, logger, reg)
	} else {
		finder = NewBucketScanBlocksFinder(BucketScanBlocksFinderConfig{
			ScanInterval:             storageCfg.BucketStore.SyncInterval,
			TenantsConcurrency:       storageCfg.BucketStore.TenantSyncConcurrency,
			MetasConcurrency:         storageCfg.BucketStore.MetaSyncConcurrency,
			CacheDir:                 storageCfg.BucketStore.SyncDir,
			IgnoreDeletionMarksDelay: storageCfg.BucketStore.IgnoreDeletionMarksDelay,
		}, bucketClient, limits, logger, reg)
	}

	storesRingCfg := gatewayCfg.ShardingRing.ToRingConfig()
	storesRingBackend, err := kv.NewClient(
		storesRingCfg.KVStore,
		ring.GetCodec(),
		kv.RegistererWithKVName(prometheus.WrapRegistererWithPrefix("cortex_", reg), "querier-store-gateway"),
		logger,
	)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create store-gateway ring backend")
	}

	storesRing, err := ring.NewWithStoreClientAndStrategy(storesRingCfg, storegateway.RingNameForClient, storegateway.RingKey, storesRingBackend, ring.NewIgnoreUnhealthyInstancesReplicationStrategy(), prometheus.WrapRegistererWithPrefix("cortex_", reg), logger)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create store-gateway ring client")
	}

	stores, err = newBlocksStoreReplicationSet(storesRing, randomLoadBalancing, limits, querierCfg.StoreGatewayClient, logger, reg)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create store set")
	}

	consistency := NewBlocksConsistencyChecker(
		// Exclude blocks which have been recently uploaded, in order to give enough time to store-gateways
		// to discover and load them (3 times the sync interval).
		storageCfg.BucketStore.ConsistencyDelay+(3*storageCfg.BucketStore.SyncInterval),
		// To avoid any false positive in the consistency check, we do exclude blocks which have been
		// recently marked for deletion, until the "ignore delay / 2". This means the consistency checker
		// exclude such blocks about 50% of the time before querier and store-gateway stops querying them.
		storageCfg.BucketStore.IgnoreDeletionMarksDelay/2,
		logger,
		reg,
	)

	return NewBlocksStoreQueryable(stores, finder, consistency, limits, querierCfg.QueryStoreAfter, logger, reg)
}

func (q *BlocksStoreQueryable) starting(ctx context.Context) error {
	q.subservicesWatcher.WatchManager(q.subservices)

	if err := services.StartManagerAndAwaitHealthy(ctx, q.subservices); err != nil {
		return errors.Wrap(err, "unable to start blocks storage queryable subservices")
	}

	return nil
}

func (q *BlocksStoreQueryable) running(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-q.subservicesWatcher.Chan():
			return errors.Wrap(err, "block storage queryable subservice failed")
		}
	}
}

func (q *BlocksStoreQueryable) stopping(_ error) error {
	return services.StopManagerAndAwaitStopped(context.Background(), q.subservices)
}

// Querier returns a new Querier on the storage.
func (q *BlocksStoreQueryable) Querier(ctx context.Context, mint, maxt int64) (storage.Querier, error) {
	if s := q.State(); s != services.Running {
		return nil, errors.Errorf("BlocksStoreQueryable is not running: %v", s)
	}

	userID, err := tenant.TenantID(ctx)
	if err != nil {
		return nil, err
	}

	return &blocksStoreQuerier{
		ctx:             ctx,
		minT:            mint,
		maxT:            maxt,
		userID:          userID,
		finder:          q.finder,
		stores:          q.stores,
		metrics:         q.metrics,
		limits:          q.limits,
		consistency:     q.consistency,
		logger:          q.logger,
		queryStoreAfter: q.queryStoreAfter,
	}, nil
}

type blocksStoreQuerier struct {
	ctx         context.Context
	minT, maxT  int64
	userID      string
	finder      BlocksFinder
	stores      BlocksStoreSet
	metrics     *blocksStoreQueryableMetrics
	consistency *BlocksConsistencyChecker
	limits      BlocksStoreLimits
	logger      log.Logger

	// If set, the querier manipulates the max time to not be greater than
	// "now - queryStoreAfter" so that most recent blocks are not queried.
	queryStoreAfter time.Duration
}

// Select implements storage.Querier interface.
// The bool passed is ignored because the series is always sorted.
func (q *blocksStoreQuerier) Select(_ bool, sp *storage.SelectHints, matchers ...*labels.Matcher) storage.SeriesSet {
	return q.selectSorted(sp, matchers...)
}

func (q *blocksStoreQuerier) LabelNames(matchers ...*labels.Matcher) ([]string, storage.Warnings, error) {
	spanLog, spanCtx := spanlogger.NewWithLogger(q.ctx, q.logger, "blocksStoreQuerier.LabelNames")
	defer spanLog.Span.Finish()

	minT, maxT := q.minT, q.maxT

	level.Debug(spanLog).Log("start", util.TimeFromMillis(minT).UTC().String(), "end",
		util.TimeFromMillis(maxT).UTC().String(), "matchers", util.MatchersStringer(matchers))

	{
		// Clamp max time range.
		startTime, endTime := model.Time(minT), model.Time(maxT)
		maxQueryLength := q.limits.MaxLabelsQueryLength(q.userID)
		minT = int64(clampTime(spanCtx, startTime, maxQueryLength, endTime.Add(-maxQueryLength), true, "start", "max label query length", spanLog))
	}

	var (
		resNameSets       = [][]string{}
		resWarnings       = storage.Warnings(nil)
		convertedMatchers = convertMatchersToLabelMatcher(matchers)
	)

	queryFunc := func(clients map[BlocksStoreClient][]ulid.ULID, minT, maxT int64) ([]ulid.ULID, error) {
		nameSets, warnings, queriedBlocks, err := q.fetchLabelNamesFromStore(spanCtx, clients, minT, maxT, convertedMatchers)
		if err != nil {
			return nil, err
		}

		resNameSets = append(resNameSets, nameSets...)
		resWarnings = append(resWarnings, warnings...)

		return queriedBlocks, nil
	}

	err := q.queryWithConsistencyCheck(spanCtx, spanLog, minT, maxT, nil, queryFunc)
	if err != nil {
		return nil, nil, err
	}

	return strutil.MergeSlices(resNameSets...), resWarnings, nil
}

func (q *blocksStoreQuerier) LabelValues(name string, matchers ...*labels.Matcher) ([]string, storage.Warnings, error) {
	spanLog, spanCtx := spanlogger.NewWithLogger(q.ctx, q.logger, "blocksStoreQuerier.LabelValues")
	defer spanLog.Span.Finish()

	minT, maxT := q.minT, q.maxT

	level.Debug(spanLog).Log("start", util.TimeFromMillis(minT).UTC().String(), "end",
		util.TimeFromMillis(maxT).UTC().String(), "matchers", util.MatchersStringer(matchers))

	{
		// Clamp max time range.
		startTime, endTime := model.Time(minT), model.Time(maxT)
		maxQueryLength := q.limits.MaxLabelsQueryLength(q.userID)
		minT = int64(clampTime(spanCtx, startTime, maxQueryLength, endTime.Add(-maxQueryLength), true, "start", "max label query length", spanLog))
	}

	var (
		resValueSets = [][]string{}
		resWarnings  = storage.Warnings(nil)
	)

	queryFunc := func(clients map[BlocksStoreClient][]ulid.ULID, minT, maxT int64) ([]ulid.ULID, error) {
		valueSets, warnings, queriedBlocks, err := q.fetchLabelValuesFromStore(spanCtx, name, clients, minT, maxT, matchers...)
		if err != nil {
			return nil, err
		}

		resValueSets = append(resValueSets, valueSets...)
		resWarnings = append(resWarnings, warnings...)

		return queriedBlocks, nil
	}

	err := q.queryWithConsistencyCheck(spanCtx, spanLog, minT, maxT, nil, queryFunc)
	if err != nil {
		return nil, nil, err
	}

	return strutil.MergeSlices(resValueSets...), resWarnings, nil
}

func (q *blocksStoreQuerier) Close() error {
	return nil
}

func (q *blocksStoreQuerier) selectSorted(sp *storage.SelectHints, matchers ...*labels.Matcher) storage.SeriesSet {
	spanLog, spanCtx := spanlogger.NewWithLogger(q.ctx, q.logger, "blocksStoreQuerier.selectSorted")
	defer spanLog.Span.Finish()

	minT, maxT := sp.Start, sp.End

	var (
		convertedMatchers = convertMatchersToLabelMatcher(matchers)
		resSeriesSets     = []storage.SeriesSet(nil)
		resWarnings       = storage.Warnings(nil)

		maxChunksLimit  = q.limits.MaxChunksPerQuery(q.userID)
		leftChunksLimit = maxChunksLimit
	)

	shard, _, err := sharding.ShardFromMatchers(matchers)
	if err != nil {
		return storage.ErrSeriesSet(err)
	}

	queryFunc := func(clients map[BlocksStoreClient][]ulid.ULID, minT, maxT int64) ([]ulid.ULID, error) {
		seriesSets, queriedBlocks, warnings, numChunks, err := q.fetchSeriesFromStores(spanCtx, sp, clients, minT, maxT, matchers, convertedMatchers, maxChunksLimit, leftChunksLimit)
		if err != nil {
			return nil, err
		}

		resSeriesSets = append(resSeriesSets, seriesSets...)
		resWarnings = append(resWarnings, warnings...)

		// Given a single block is guaranteed to not be queried twice, we can safely decrease the number of
		// chunks we can still read before hitting the limit (max == 0 means disabled).
		if maxChunksLimit > 0 {
			leftChunksLimit -= numChunks
		}

		return queriedBlocks, nil
	}

	err = q.queryWithConsistencyCheck(spanCtx, spanLog, minT, maxT, shard, queryFunc)
	if err != nil {
		return storage.ErrSeriesSet(err)
	}

	if len(resSeriesSets) == 0 {
		storage.EmptySeriesSet()
	}

	return series.NewSeriesSetWithWarnings(
		storage.NewMergeSeriesSet(resSeriesSets, storage.ChainedSeriesMerge),
		resWarnings)
}

func (q *blocksStoreQuerier) queryWithConsistencyCheck(ctx context.Context, logger log.Logger, minT, maxT int64, shard *sharding.ShardSelector,
	queryFunc func(clients map[BlocksStoreClient][]ulid.ULID, minT, maxT int64) ([]ulid.ULID, error)) error {
	// If queryStoreAfter is enabled, we do manipulate the query maxt to query samples up until
	// now - queryStoreAfter, because the most recent time range is covered by ingesters. This
	// optimization is particularly important for the blocks storage because can be used to skip
	// querying most recent not-compacted-yet blocks from the storage.
	if q.queryStoreAfter > 0 {
		now := time.Now()
		origMaxT := maxT
		maxT = math.Min64(maxT, util.TimeToMillis(now.Add(-q.queryStoreAfter)))

		if origMaxT != maxT {
			level.Debug(logger).Log("msg", "the max time of the query to blocks storage has been manipulated", "original", origMaxT, "updated", maxT)
		}

		if maxT < minT {
			q.metrics.storesHit.Observe(0)
			level.Debug(logger).Log("msg", "empty query time range after max time manipulation")
			return nil
		}
	}

	// Find the list of blocks we need to query given the time range.
	knownBlocks, knownDeletionMarks, err := q.finder.GetBlocks(ctx, q.userID, minT, maxT)
	if err != nil {
		return err
	}

	if len(knownBlocks) == 0 {
		q.metrics.storesHit.Observe(0)
		level.Debug(logger).Log("msg", "no blocks found")
		return nil
	}

	q.metrics.blocksFound.Add(float64(len(knownBlocks)))

	if shard != nil && shard.ShardCount > 0 {
		level.Debug(logger).Log("msg", "filtering blocks due to sharding", "blocksBeforeFiltering", knownBlocks.String(), "shardID", shard.LabelValue())

		result, incompatibleBlocks := filterBlocksByShard(knownBlocks, shard.ShardIndex, shard.ShardCount)

		level.Debug(logger).Log("msg", "result of filtering blocks", "before", len(knownBlocks), "after", len(result), "filtered", len(knownBlocks)-len(result), "incompatible", incompatibleBlocks)
		q.metrics.blocksWithCompactorShardButIncompatibleQueryShard.Add(float64(incompatibleBlocks))

		knownBlocks = result
	}

	q.metrics.blocksQueried.Add(float64(len(knownBlocks)))

	level.Debug(logger).Log("msg", "found blocks to query", "expected", knownBlocks.String())

	var (
		// At the beginning the list of blocks to query are all known blocks.
		remainingBlocks = knownBlocks.GetULIDs()
		attemptedBlocks = map[ulid.ULID][]string{}
		touchedStores   = map[string]struct{}{}

		resQueriedBlocks = []ulid.ULID(nil)
	)

	for attempt := 1; attempt <= maxFetchSeriesAttempts; attempt++ {
		// Find the set of store-gateway instances having the blocks. The exclude parameter is the
		// map of blocks queried so far, with the list of store-gateway addresses for each block.
		clients, err := q.stores.GetClientsFor(q.userID, remainingBlocks, attemptedBlocks)
		if err != nil {
			// If it's a retry and we get an error, it means there are no more store-gateways left
			// from which running another attempt, so we're just stopping retrying.
			if attempt > 1 {
				level.Warn(logger).Log("msg", "unable to get store-gateway clients while retrying to fetch missing blocks", "err", err)
				break
			}

			return err
		}
		level.Debug(logger).Log("msg", "found store-gateway instances to query", "num instances", len(clients), "attempt", attempt)

		// Fetch series from stores. If an error occur we do not retry because retries
		// are only meant to cover missing blocks.
		queriedBlocks, err := queryFunc(clients, minT, maxT)
		if err != nil {
			return err
		}
		level.Debug(logger).Log("msg", "received series from all store-gateways", "queried blocks", strings.Join(convertULIDsToString(queriedBlocks), " "))

		resQueriedBlocks = append(resQueriedBlocks, queriedBlocks...)

		// Update the map of blocks we attempted to query.
		for client, blockIDs := range clients {
			touchedStores[client.RemoteAddress()] = struct{}{}

			for _, blockID := range blockIDs {
				attemptedBlocks[blockID] = append(attemptedBlocks[blockID], client.RemoteAddress())
			}
		}

		// Ensure all expected blocks have been queried (during all tries done so far).
		missingBlocks := q.consistency.Check(knownBlocks, knownDeletionMarks, resQueriedBlocks)
		if len(missingBlocks) == 0 {
			q.metrics.storesHit.Observe(float64(len(touchedStores)))
			q.metrics.refetches.Observe(float64(attempt - 1))

			return nil
		}

		level.Debug(logger).Log("msg", "consistency check failed", "attempt", attempt, "missing blocks", strings.Join(convertULIDsToString(missingBlocks), " "))

		// The next attempt should just query the missing blocks.
		remainingBlocks = missingBlocks
	}

	// We've not been able to query all expected blocks after all retries.
	level.Warn(util_log.WithContext(ctx, logger)).Log("msg", "failed consistency check", "err", err)
	return newStoreConsistencyCheckFailedError(remainingBlocks)
}

func newStoreConsistencyCheckFailedError(remainingBlocks []ulid.ULID) error {
	return fmt.Errorf("%v. The non-queried blocks are: %s", globalerror.StoreConsistencyCheckFailed.Message("the consistency check failed because some blocks were not queried"), strings.Join(convertULIDsToString(remainingBlocks), " "))
}

// filterBlocksByShard removes blocks that can be safely ignored when using query sharding. We know that block can be safely
// ignored, if it was compacted using split-and-merge compactor, and it has a valid compactor shard ID. We exploit the
// fact that split-and-merge compactor and query-sharding use the same series-sharding algorithm.
//
// This function modifies input slice.
//
// This function also returns number of "incompatible" blocks -- blocks with compactor shard ID, but with compactor shard
// and query shard being incompatible for optimization.
func filterBlocksByShard(blocks bucketindex.Blocks, queryShardIndex, queryShardCount uint64) (_ bucketindex.Blocks, incompatibleBlocks int) {
	for ix := 0; ix < len(blocks); {
		b := blocks[ix]
		if b.CompactorShardID == "" {
			ix++
			continue
		}

		compactorShardIndex, compactorShardCount, err := sharding.ParseShardIDLabelValue(b.CompactorShardID)
		if err != nil {
			// Cannot parse compactor shardID, we must query this block.
			ix++
			continue
		}

		res, divisible := canBlockWithCompactorShardIndexContainQueryShard(queryShardIndex, queryShardCount, compactorShardIndex, compactorShardCount)
		if !divisible {
			incompatibleBlocks++
		}

		if res {
			ix++
			continue
		}

		// Series shard is NOT included in this block, we can remove this block.
		blocks = append(blocks[:ix], blocks[ix+1:]...)
	}

	return blocks, incompatibleBlocks
}

// canBlockWithCompactorShardIndexContainQueryShard returns false if block with given compactor shard ID can *definitely NOT*
// contain series for given query shard. Returns true otherwise (we don't know if block *does* contain such series,
// but we cannot rule it out).
//
// In other words, if this function returns false, block with given compactorShardID doesn't need to be searched for series from given query shard.
//
// In addition this function also returns whether query and compactor shard counts were divisible by each other (one way or the other).
func canBlockWithCompactorShardIndexContainQueryShard(queryShardIndex, queryShardCount, compactorShardIndex, compactorShardCount uint64) (result bool, divisibleShardCounts bool) {
	// If queryShardCount = compactorShardCount * K for integer K, then we know that series in queryShardIndex
	// can only be in the block for which (queryShardIndex % compactorShardCount == compactorShardIndex).
	//
	// For example if queryShardCount = 8 and compactorShardCount = 4, then series that should be returned
	// for queryShardIndex 5 can only be in block with compactorShardIndex = 1.
	if queryShardCount >= compactorShardCount && queryShardCount%compactorShardCount == 0 {
		wantedCompactorShardIndex := queryShardIndex % compactorShardCount

		return compactorShardIndex == wantedCompactorShardIndex, true
	}

	// If compactorShardCount = queryShardCount * K for some integer K, then series in queryShardIndex
	// can only be in K blocks for which queryShardIndex % compactorShardCount == compactorShardIndex.
	//
	// For example if queryShardCount = 4, and compactorShardCount = 8, then series that should be returned for
	// queryShardIndex 3 can only be in blocks with compactorShardIndex 3 and 7.
	if compactorShardCount >= queryShardCount && compactorShardCount%queryShardCount == 0 {
		wantedQueryShardIndex := compactorShardIndex % queryShardCount

		return queryShardIndex == wantedQueryShardIndex, true
	}

	return true, false
}

func (q *blocksStoreQuerier) fetchSeriesFromStores(
	ctx context.Context,
	sp *storage.SelectHints,
	clients map[BlocksStoreClient][]ulid.ULID,
	minT int64,
	maxT int64,
	matchers []*labels.Matcher,
	convertedMatchers []storepb.LabelMatcher,
	maxChunksLimit int,
	leftChunksLimit int,
) ([]storage.SeriesSet, []ulid.ULID, storage.Warnings, int, error) {
	var (
		reqCtx        = grpc_metadata.AppendToOutgoingContext(ctx, storegateway.GrpcContextMetadataTenantID, q.userID)
		g, gCtx       = errgroup.WithContext(reqCtx)
		mtx           = sync.Mutex{}
		seriesSets    = []storage.SeriesSet(nil)
		warnings      = storage.Warnings(nil)
		queriedBlocks = []ulid.ULID(nil)
		numChunks     = atomic.NewInt32(0)
		spanLog       = spanlogger.FromContext(ctx, q.logger)
		queryLimiter  = limiter.QueryLimiterFromContextWithFallback(ctx)
		reqStats      = stats.FromContext(ctx)
	)

	// Concurrently fetch series from all clients.
	for c, blockIDs := range clients {
		// Change variables scope since it will be used in a goroutine.
		c := c
		blockIDs := blockIDs

		g.Go(func() error {
			// See: https://github.com/prometheus/prometheus/pull/8050
			// TODO(goutham): we should ideally be passing the hints down to the storage layer
			// and let the TSDB return us data with no chunks as in prometheus#8050.
			// But this is an acceptable workaround for now.
			skipChunks := sp != nil && sp.Func == "series"

			req, err := createSeriesRequest(minT, maxT, convertedMatchers, skipChunks, blockIDs)
			if err != nil {
				return errors.Wrapf(err, "failed to create series request")
			}

			stream, err := c.Series(gCtx, req)
			if err != nil {
				level.Warn(spanLog).Log("msg", "failed to fetch series", "remote", c.RemoteAddress(), "err", err)
				return nil
			}

			mySeries := []*storepb.Series(nil)
			myWarnings := storage.Warnings(nil)
			myQueriedBlocks := []ulid.ULID(nil)

			for {
				// Ensure the context hasn't been canceled in the meanwhile (eg. an error occurred
				// in another goroutine).
				if gCtx.Err() != nil {
					return gCtx.Err()
				}

				resp, err := stream.Recv()
				if err == io.EOF {
					break
				}
				if err != nil {
					level.Warn(spanLog).Log("msg", "failed to receive series", "remote", c.RemoteAddress(), "err", err)
					return nil
				}

				// Response may either contain series, warning or hints.
				if s := resp.GetSeries(); s != nil {
					mySeries = append(mySeries, s)

					// Add series fingerprint to query limiter; will return error if we are over the limit
					limitErr := queryLimiter.AddSeries(mimirpb.FromLabelsToLabelAdapters(s.PromLabels()))
					if limitErr != nil {
						return validation.LimitError(limitErr.Error())
					}

					chunksCount, chunksSize := countChunksAndBytes(s)

					// Ensure the max number of chunks limit hasn't been reached (max == 0 means disabled).
					if maxChunksLimit > 0 {
						actual := numChunks.Add(int32(chunksCount))
						if actual > int32(leftChunksLimit) {
							return validation.LimitError(fmt.Sprintf(maxChunksPerQueryLimitMsgFormat, util.LabelMatchersToString(matchers), maxChunksLimit))
						}
					}
					if chunkBytesLimitErr := queryLimiter.AddChunkBytes(chunksSize); chunkBytesLimitErr != nil {
						return validation.LimitError(chunkBytesLimitErr.Error())
					}
					if chunkLimitErr := queryLimiter.AddChunks(len(s.Chunks)); chunkLimitErr != nil {
						return validation.LimitError(chunkLimitErr.Error())
					}
				}

				if w := resp.GetWarning(); w != "" {
					myWarnings = append(myWarnings, errors.New(w))
				}

				if h := resp.GetHints(); h != nil {
					hints := hintspb.SeriesResponseHints{}
					if err := types.UnmarshalAny(h, &hints); err != nil {
						return errors.Wrapf(err, "failed to unmarshal series hints from %s", c.RemoteAddress())
					}

					ids, err := convertBlockHintsToULIDs(hints.QueriedBlocks)
					if err != nil {
						return errors.Wrapf(err, "failed to parse queried block IDs from received hints")
					}

					myQueriedBlocks = append(myQueriedBlocks, ids...)
				}
			}

			numSeries := len(mySeries)
			chunksFetched, chunkBytes := countChunksAndBytes(mySeries...)

			reqStats.AddFetchedSeries(uint64(numSeries))
			reqStats.AddFetchedChunkBytes(uint64(chunkBytes))
			reqStats.AddFetchedChunks(uint64(chunksFetched))

			level.Debug(spanLog).Log("msg", "received series from store-gateway",
				"instance", c.RemoteAddress(),
				"fetched series", numSeries,
				"fetched chunk bytes", chunkBytes,
				"fetched chunks", chunksFetched,
				"requested blocks", strings.Join(convertULIDsToString(blockIDs), " "),
				"queried blocks", strings.Join(convertULIDsToString(myQueriedBlocks), " "))

			// Store the result.
			mtx.Lock()
			seriesSets = append(seriesSets, &blockQuerierSeriesSet{series: mySeries})
			warnings = append(warnings, myWarnings...)
			queriedBlocks = append(queriedBlocks, myQueriedBlocks...)
			mtx.Unlock()

			return nil
		})
	}

	// Wait until all client requests complete.
	if err := g.Wait(); err != nil {
		return nil, nil, nil, 0, err
	}

	return seriesSets, queriedBlocks, warnings, int(numChunks.Load()), nil
}

func (q *blocksStoreQuerier) fetchLabelNamesFromStore(
	ctx context.Context,
	clients map[BlocksStoreClient][]ulid.ULID,
	minT int64,
	maxT int64,
	matchers []storepb.LabelMatcher,
) ([][]string, storage.Warnings, []ulid.ULID, error) {
	var (
		reqCtx        = grpc_metadata.AppendToOutgoingContext(ctx, storegateway.GrpcContextMetadataTenantID, q.userID)
		g, gCtx       = errgroup.WithContext(reqCtx)
		mtx           = sync.Mutex{}
		nameSets      = [][]string{}
		warnings      = storage.Warnings(nil)
		queriedBlocks = []ulid.ULID(nil)
		spanLog       = spanlogger.FromContext(ctx, q.logger)
	)

	// Concurrently fetch series from all clients.
	for c, blockIDs := range clients {
		// Change variables scope since it will be used in a goroutine.
		c := c
		blockIDs := blockIDs

		g.Go(func() error {
			req, err := createLabelNamesRequest(minT, maxT, blockIDs, matchers)
			if err != nil {
				return errors.Wrapf(err, "failed to create label names request")
			}

			namesResp, err := c.LabelNames(gCtx, req)
			if err != nil {
				level.Warn(spanLog).Log("msg", "failed to fetch label names", "remote", c.RemoteAddress(), "err", err)
				return nil
			}

			myQueriedBlocks := []ulid.ULID(nil)
			if namesResp.Hints != nil {
				hints := hintspb.LabelNamesResponseHints{}
				if err := types.UnmarshalAny(namesResp.Hints, &hints); err != nil {
					return errors.Wrapf(err, "failed to unmarshal label names hints from %s", c.RemoteAddress())
				}

				ids, err := convertBlockHintsToULIDs(hints.QueriedBlocks)
				if err != nil {
					return errors.Wrapf(err, "failed to parse queried block IDs from received hints")
				}

				myQueriedBlocks = ids
			}

			level.Debug(spanLog).Log("msg", "received label names from store-gateway",
				"instance", c,
				"num labels", len(namesResp.Names),
				"requested blocks", strings.Join(convertULIDsToString(blockIDs), " "),
				"queried blocks", strings.Join(convertULIDsToString(myQueriedBlocks), " "))

			// Store the result.
			mtx.Lock()
			nameSets = append(nameSets, namesResp.Names)
			for _, w := range namesResp.Warnings {
				warnings = append(warnings, errors.New(w))
			}
			queriedBlocks = append(queriedBlocks, myQueriedBlocks...)
			mtx.Unlock()

			return nil
		})
	}

	// Wait until all client requests complete.
	if err := g.Wait(); err != nil {
		return nil, nil, nil, err
	}

	return nameSets, warnings, queriedBlocks, nil
}

func (q *blocksStoreQuerier) fetchLabelValuesFromStore(
	ctx context.Context,
	name string,
	clients map[BlocksStoreClient][]ulid.ULID,
	minT int64,
	maxT int64,
	matchers ...*labels.Matcher,
) ([][]string, storage.Warnings, []ulid.ULID, error) {
	var (
		reqCtx        = grpc_metadata.AppendToOutgoingContext(ctx, storegateway.GrpcContextMetadataTenantID, q.userID)
		g, gCtx       = errgroup.WithContext(reqCtx)
		mtx           = sync.Mutex{}
		valueSets     = [][]string{}
		warnings      = storage.Warnings(nil)
		queriedBlocks = []ulid.ULID(nil)
		spanLog       = spanlogger.FromContext(ctx, q.logger)
	)

	// Concurrently fetch series from all clients.
	for c, blockIDs := range clients {
		// Change variables scope since it will be used in a goroutine.
		c := c
		blockIDs := blockIDs

		g.Go(func() error {
			req, err := createLabelValuesRequest(minT, maxT, name, blockIDs, matchers...)
			if err != nil {
				return errors.Wrapf(err, "failed to create label values request")
			}

			valuesResp, err := c.LabelValues(gCtx, req)
			if err != nil {
				level.Warn(spanLog).Log("msg", "failed to fetch label values", "remote", c.RemoteAddress(), "err", err)
				return nil
			}

			myQueriedBlocks := []ulid.ULID(nil)
			if valuesResp.Hints != nil {
				hints := hintspb.LabelValuesResponseHints{}
				if err := types.UnmarshalAny(valuesResp.Hints, &hints); err != nil {
					return errors.Wrapf(err, "failed to unmarshal label values hints from %s", c.RemoteAddress())
				}

				ids, err := convertBlockHintsToULIDs(hints.QueriedBlocks)
				if err != nil {
					return errors.Wrapf(err, "failed to parse queried block IDs from received hints")
				}

				myQueriedBlocks = ids
			}

			level.Debug(spanLog).Log("msg", "received label values from store-gateway",
				"instance", c.RemoteAddress(),
				"num values", len(valuesResp.Values),
				"requested blocks", strings.Join(convertULIDsToString(blockIDs), " "),
				"queried blocks", strings.Join(convertULIDsToString(myQueriedBlocks), " "))

			// Values returned need not be sorted, but we need them to be sorted so we can merge.
			sort.Strings(valuesResp.Values)

			// Store the result.
			mtx.Lock()
			valueSets = append(valueSets, valuesResp.Values)
			for _, w := range valuesResp.Warnings {
				warnings = append(warnings, errors.New(w))
			}
			queriedBlocks = append(queriedBlocks, myQueriedBlocks...)
			mtx.Unlock()

			return nil
		})
	}

	// Wait until all client requests complete.
	if err := g.Wait(); err != nil {
		return nil, nil, nil, err
	}

	return valueSets, warnings, queriedBlocks, nil
}

func createSeriesRequest(minT, maxT int64, matchers []storepb.LabelMatcher, skipChunks bool, blockIDs []ulid.ULID) (*storepb.SeriesRequest, error) {
	// Selectively query only specific blocks.
	hints := &hintspb.SeriesRequestHints{
		BlockMatchers: []storepb.LabelMatcher{
			{
				Type:  storepb.LabelMatcher_RE,
				Name:  block.BlockIDLabel,
				Value: strings.Join(convertULIDsToString(blockIDs), "|"),
			},
		},
	}

	anyHints, err := types.MarshalAny(hints)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to marshal series request hints")
	}

	return &storepb.SeriesRequest{
		MinTime:                 minT,
		MaxTime:                 maxT,
		Matchers:                matchers,
		PartialResponseStrategy: storepb.PartialResponseStrategy_ABORT,
		Hints:                   anyHints,
		SkipChunks:              skipChunks,
	}, nil
}

func createLabelNamesRequest(minT, maxT int64, blockIDs []ulid.ULID, matchers []storepb.LabelMatcher) (*storepb.LabelNamesRequest, error) {
	req := &storepb.LabelNamesRequest{
		Start:    minT,
		End:      maxT,
		Matchers: matchers,
	}

	// Selectively query only specific blocks.
	hints := &hintspb.LabelNamesRequestHints{
		BlockMatchers: []storepb.LabelMatcher{
			{
				Type:  storepb.LabelMatcher_RE,
				Name:  block.BlockIDLabel,
				Value: strings.Join(convertULIDsToString(blockIDs), "|"),
			},
		},
	}

	anyHints, err := types.MarshalAny(hints)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to marshal label names request hints")
	}

	req.Hints = anyHints

	return req, nil
}

func createLabelValuesRequest(minT, maxT int64, label string, blockIDs []ulid.ULID, matchers ...*labels.Matcher) (*storepb.LabelValuesRequest, error) {
	req := &storepb.LabelValuesRequest{
		Start:    minT,
		End:      maxT,
		Label:    label,
		Matchers: convertMatchersToLabelMatcher(matchers),
	}

	// Selectively query only specific blocks.
	hints := &hintspb.LabelValuesRequestHints{
		BlockMatchers: []storepb.LabelMatcher{
			{
				Type:  storepb.LabelMatcher_RE,
				Name:  block.BlockIDLabel,
				Value: strings.Join(convertULIDsToString(blockIDs), "|"),
			},
		},
	}

	anyHints, err := types.MarshalAny(hints)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to marshal label values request hints")
	}

	req.Hints = anyHints

	return req, nil
}

func convertULIDsToString(ids []ulid.ULID) []string {
	res := make([]string, len(ids))
	for idx, id := range ids {
		res[idx] = id.String()
	}
	return res
}

func convertBlockHintsToULIDs(hints []hintspb.Block) ([]ulid.ULID, error) {
	res := make([]ulid.ULID, len(hints))

	for idx, hint := range hints {
		blockID, err := ulid.Parse(hint.Id)
		if err != nil {
			return nil, err
		}

		res[idx] = blockID
	}

	return res, nil
}

// countChunksAndBytes returns the number of chunks and size of the chunks making up the provided series in bytes
func countChunksAndBytes(series ...*storepb.Series) (chunks, bytes int) {
	for _, s := range series {
		chunks += len(s.Chunks)
		for _, c := range s.Chunks {
			bytes += c.Size()
		}
	}

	return chunks, bytes
}
