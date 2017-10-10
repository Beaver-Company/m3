package main

import (
	"flag"
	"io"
	"net/http"
	_ "net/http/pprof"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/m3db/m3x/pool"

	"github.com/m3db/m3db/encoding"
	"github.com/m3db/m3db/encoding/m3tsz"
	"github.com/m3db/m3db/persist/fs"
	"github.com/m3db/m3db/persist/fs/commitlog"
	"github.com/m3db/m3db/retention"
	"github.com/m3db/m3db/storage/block"
	"github.com/m3db/m3db/storage/bootstrap"
	"github.com/m3db/m3db/storage/bootstrap/bootstrapper"
	commitlogsrc "github.com/m3db/m3db/storage/bootstrap/bootstrapper/commitlog"
	"github.com/m3db/m3db/storage/bootstrap/result"
	"github.com/m3db/m3db/storage/namespace"
	"github.com/m3db/m3db/ts"
	"github.com/m3db/m3db/x/io"
	"github.com/m3db/m3x/instrument"
	xlog "github.com/m3db/m3x/log"
	xtime "github.com/m3db/m3x/time"
)

var (
	pathPrefixArg           = flag.String("path-prefix", "/var/lib/m3db", "Path prefix - must contain a folder called 'commitlogs'")
	namespaceArg            = flag.String("namespace", "metrics", "Namespace")
	blockSizeArg            = flag.Duration("block-size", 10*time.Minute, "Block size")
	flushSizeArg            = flag.Int("flush-size", 524288, "Flush size of commit log")
	bootstrapRetentionArg   = flag.Duration("retention", 48*time.Hour, "Retention")
	shardsCountArg          = flag.Int("shards-count", 8192, "Shards count - set number too bootstrap all shards in range")
	shardsArg               = flag.String("shards", "", "Shards - set comma separated list of shards")
	debugListenAddressArg   = flag.String("debug-listen-address", "", "Debug listen address - if set will expose pprof, i.e. ':8080'")
	currentUnixTimestampArg = flag.Int64("current-unix-timestamp", time.Now().Unix(), "Current unix timestamp (Seconds) - If set will perform the bootstrap as if this was the current time, defaults to current time")
)

func main() {
	flag.Parse()
	if *pathPrefixArg == "" ||
		*namespaceArg == "" {
		flag.Usage()
		os.Exit(1)
	}

	var (
		pathPrefix           = *pathPrefixArg
		namespaceStr         = *namespaceArg
		blockSize            = *blockSizeArg
		flushSize            = *flushSizeArg
		bootstrapRetention   = *bootstrapRetentionArg
		shardsCount          = *shardsCountArg
		shards               = *shardsArg
		debugListenAddress   = *debugListenAddressArg
		currentUnixTimestamp = *currentUnixTimestampArg
	)

	log := xlog.NewLogger(os.Stderr)

	if debugListenAddress != "" {
		go func() {
			log.Infof("starting debug listen server at '%s'\n", debugListenAddress)
			err := http.ListenAndServe(debugListenAddress, http.DefaultServeMux)
			if err != nil {
				log.Fatalf("could not start debug listen server at '%s': %v", debugListenAddress, err)
			}
		}()
	}

	shardTimeRanges := result.ShardTimeRanges{}

	now := time.Unix(currentUnixTimestamp, 0)
	// Round current time down to nearest blocksize (2h) and then decrease blocksize (2h)
	startInclusive := now.Truncate(blockSize).Add(-blockSize)
	// Round current time down to nearest blocksize (2h) and then add blocksize (2h)
	endExclusive := now.Truncate(blockSize).Add(blockSize * 2)

	// Ony used for logging
	var shardsAll []uint32

	// Handle commda-delimited shard list 1,3,5, etc
	if strings.TrimSpace(shards) != "" {
		for _, shard := range strings.Split(shards, ",") {
			shard = strings.TrimSpace(shard)
			if shard == "" {
				log.Fatalf("Invalid shard list: '%s'", shards)
			}
			value, err := strconv.Atoi(shard)
			if err != nil {
				log.Fatalf("could not parse shard '%s': %v", shard, err)
			}
			rng := xtime.Range{Start: startInclusive, End: endExclusive}
			shardTimeRanges[uint32(value)] = xtime.NewRanges().AddRange(rng)
			shardsAll = append(shardsAll, uint32(value))
		}
		// Or just handled up to N (shard-count) shards
	} else if shardsCount > 0 {
		for i := uint32(0); i < uint32(shardsCount); i++ {
			rng := xtime.Range{Start: startInclusive, End: endExclusive}
			shardTimeRanges[i] = xtime.NewRanges().AddRange(rng)
			shardsAll = append(shardsAll, i)
		}
	} else {
		log.Info("Either the shards or shards-count argument need to be valid")
		flag.Usage()
		os.Exit(1)
	}

	log.WithFields(
		xlog.NewField("pathPrefix", pathPrefix),
		xlog.NewField("namespace", namespaceStr),
		xlog.NewField("shards", shardsAll),
	).Infof("configured")

	instrumentOpts := instrument.NewOptions().
		SetLogger(log)

	retentionOpts := retention.NewOptions().
		SetBlockSize(blockSize).
		SetRetentionPeriod(bootstrapRetention).
		SetBufferPast(1 * time.Minute).
		SetBufferFuture(1 * time.Minute)

	blockOpts := block.NewOptions()

	encoderPoolOpts := pool.
		NewObjectPoolOptions().
		SetSize(25165824).
		SetRefillLowWatermark(0.001).
		SetRefillHighWatermark(0.002)
	encoderPool := encoding.NewEncoderPool(encoderPoolOpts)

	iteratorPoolOpts := pool.NewObjectPoolOptions().
		SetSize(2048).
		SetRefillLowWatermark(0.01).
		SetRefillHighWatermark(0.02)
	iteratorPool := encoding.NewReaderIteratorPool(iteratorPoolOpts)

	multiIteratorPool := encoding.NewMultiReaderIteratorPool(nil)
	segmentReaderPool := xio.NewSegmentReaderPool(nil)

	encodingOpts := encoding.NewOptions().
		SetEncoderPool(encoderPool).
		SetReaderIteratorPool(iteratorPool).
		SetBytesPool(blockOpts.BytesPool()).
		SetSegmentReaderPool(segmentReaderPool)

	encoderPool.Init(func() encoding.Encoder {
		return m3tsz.NewEncoder(time.Time{}, nil, true, encodingOpts)
	})

	iteratorPool.Init(func(r io.Reader) encoding.ReaderIterator {
		return m3tsz.NewReaderIterator(r, true, encodingOpts)
	})

	multiIteratorPool.Init(func(r io.Reader) encoding.ReaderIterator {
		iter := iteratorPool.Get()
		iter.Reset(r)
		return iter
	})

	segmentReaderPool.Init()

	blockPool := block.NewDatabaseBlockPool(nil)
	blockPool.Init(func() block.DatabaseBlock {
		return block.NewDatabaseBlock(time.Time{}, ts.Segment{}, blockOpts)
	})

	blockOpts = blockOpts.
		SetEncoderPool(encoderPool).
		SetReaderIteratorPool(iteratorPool).
		SetMultiReaderIteratorPool(multiIteratorPool).
		SetDatabaseBlockPool(blockPool).
		SetSegmentReaderPool(segmentReaderPool)

	resultOpts := result.NewOptions().
		SetInstrumentOptions(instrumentOpts).
		SetDatabaseBlockOptions(blockOpts)

	fsOpts := fs.NewOptions().
		SetInstrumentOptions(instrumentOpts).
		SetFilePathPrefix(pathPrefix)

	commitLogOpts := commitlog.NewOptions().
		SetInstrumentOptions(instrumentOpts).
		SetFilesystemOptions(fsOpts).
		SetFlushSize(flushSize).
		SetBlockSize(blockSize)

	opts := commitlogsrc.NewOptions().
		SetResultOptions(resultOpts).
		SetCommitLogOptions(commitLogOpts)

	log.Infof("bootstrapping")

	// Don't bootstrap anything else
	next := bootstrapper.NewNoOpAllBootstrapper()
	source, err := commitlogsrc.NewCommitLogBootstrapper(opts, next)
	if err != nil {
		log.Fatal(err.Error())
	}

	nsID := ts.StringID(namespaceStr)
	runOpts := bootstrap.NewRunOptions().
		// Dont save intermediate results
		SetIncremental(false)
	nsMetadata, err := namespace.NewMetadata(nsID, namespace.NewOptions().SetRetentionOptions(retentionOpts))
	if err != nil {
		log.Fatal(err.Error())
	}
	result, err := source.Bootstrap(nsMetadata, shardTimeRanges, runOpts)
	if err != nil {
		log.Fatalf("failed to bootstrap: %v", err)
	}

	log.WithFields(
		xlog.NewField("shardResults", len(result.ShardResults())),
		xlog.NewField("unfulfilled", len(result.Unfulfilled())),
	).Infof("bootstrapped")

	for shard, result := range result.ShardResults() {
		log.WithFields(
			xlog.NewField("shard", shard),
			xlog.NewField("series", len(result.AllSeries())),
		).Infof("shard result")
	}
}