package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/DefiantLabs/probe/client"

	"github.com/DefiantLabs/cosmos-indexer/config"
	"github.com/DefiantLabs/cosmos-indexer/core"
	dbTypes "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"github.com/DefiantLabs/cosmos-indexer/filter"
	"github.com/DefiantLabs/cosmos-indexer/parsers"
	"github.com/DefiantLabs/cosmos-indexer/probe"
	"github.com/DefiantLabs/cosmos-indexer/rpc"
	"github.com/cosmos/cosmos-sdk/types/module"
	"github.com/spf13/cobra"
	"gorm.io/gorm"
)

type Indexer struct {
	cfg                                 *config.IndexConfig
	dryRun                              bool
	db                                  *gorm.DB
	cl                                  *client.ChainClient
	blockEnqueueFunction                func(chan *core.EnqueueData) error
	customModuleBasics                  []module.AppModuleBasic // Used for extending the AppModuleBasics registered in the probe client
	blockEventFilterRegistries          blockEventFilterRegistries
	messageTypeFilters                  []filter.MessageTypeFilter
	customBeginBlockEventParserRegistry map[string][]parsers.BlockEventParser // Used for associating parsers to block event types in BeginBlock events
	customEndBlockEventParserRegistry   map[string][]parsers.BlockEventParser // Used for associating parsers to block event types in EndBlock events
	customBeginBlockParserTrackers      map[string]models.BlockEventParser    // Used for tracking block event parsers in the database
	customEndBlockParserTrackers        map[string]models.BlockEventParser    // Used for tracking block event parsers in the database
	customMessageParserRegistry         map[string][]parsers.MessageParser    // Used for associating parsers to message types
	customMessageParserTrackers         map[string]models.MessageParser       // Used for tracking message parsers in the database
	customModels                        []any
}

type blockEventFilterRegistries struct {
	beginBlockEventFilterRegistry *filter.StaticBlockEventFilterRegistry
	endBlockEventFilterRegistry   *filter.StaticBlockEventFilterRegistry
}

var indexer Indexer

func init() {
	indexer.cfg = &config.IndexConfig{}
	config.SetupLogFlags(&indexer.cfg.Log, indexCmd)
	config.SetupDatabaseFlags(&indexer.cfg.Database, indexCmd)
	config.SetupProbeFlags(&indexer.cfg.Probe, indexCmd)
	config.SetupThrottlingFlag(&indexer.cfg.Base.Throttling, indexCmd)
	config.SetupIndexSpecificFlags(indexer.cfg, indexCmd)

	rootCmd.AddCommand(indexCmd)
}

var indexCmd = &cobra.Command{
	Use:   "index",
	Short: "Indexes the blockchain according to the configuration defined.",
	Long: `Indexes the Cosmos-based blockchain according to the configurations found on the command line
	or in the specified config file. Indexes taxable events into a database for easy querying. It is
	highly recommended to keep this command running as a background service to keep your index up to date.`,
	PreRunE: setupIndex,
	Run:     index,
}

func RegisterCustomModuleBasics(basics []module.AppModuleBasic) {
	indexer.customModuleBasics = append(indexer.customModuleBasics, basics...)
}

func RegisterMessageTypeFilter(filter filter.MessageTypeFilter) {
	indexer.messageTypeFilters = append(indexer.messageTypeFilters, filter)
}

func RegisterCustomBeginBlockEventParser(eventKey string, parser parsers.BlockEventParser) {
	var err error
	indexer.customBeginBlockEventParserRegistry, indexer.customBeginBlockParserTrackers, err = customBlockEventRegistration(
		indexer.customBeginBlockEventParserRegistry,
		indexer.customBeginBlockParserTrackers,
		eventKey,
		parser,
		models.BeginBlockEvent,
	)

	if err != nil {
		config.Log.Fatal("Error registering BeginBlock custom parser", err)
	}
}

func RegisterCustomEndBlockEventParser(eventKey string, parser parsers.BlockEventParser) {
	var err error
	indexer.customEndBlockEventParserRegistry, indexer.customEndBlockParserTrackers, err = customBlockEventRegistration(
		indexer.customEndBlockEventParserRegistry,
		indexer.customEndBlockParserTrackers,
		eventKey,
		parser,
		models.EndBlockEvent,
	)

	if err != nil {
		config.Log.Fatal("Error registering EndBlock custom parser", err)
	}
}

func RegisterCustomMessageParser(messageKey string, parser parsers.MessageParser) {
	if indexer.customMessageParserRegistry == nil {
		indexer.customMessageParserRegistry = make(map[string][]parsers.MessageParser)
	}

	if indexer.customMessageParserTrackers == nil {
		indexer.customMessageParserTrackers = make(map[string]models.MessageParser)
	}

	indexer.customMessageParserRegistry[messageKey] = append(indexer.customMessageParserRegistry[messageKey], parser)

	if _, ok := indexer.customMessageParserTrackers[parser.Identifier()]; ok {
		config.Log.Fatalf("Found duplicate message parser with identifier \"%s\", parsers must be uniquely identified", parser.Identifier())
	}

	indexer.customMessageParserTrackers[parser.Identifier()] = models.MessageParser{
		Identifier: parser.Identifier(),
	}
}

func customBlockEventRegistration(registry map[string][]parsers.BlockEventParser, tracker map[string]models.BlockEventParser, eventKey string, parser parsers.BlockEventParser, lifecycleValue models.BlockLifecyclePosition) (map[string][]parsers.BlockEventParser, map[string]models.BlockEventParser, error) {
	if registry == nil {
		registry = make(map[string][]parsers.BlockEventParser)
	}

	if tracker == nil {
		tracker = make(map[string]models.BlockEventParser)
	}

	registry[eventKey] = append(registry[eventKey], parser)

	if _, ok := tracker[parser.Identifier()]; ok {
		return registry, tracker, fmt.Errorf("found duplicate block event parser with identifier \"%s\", parsers must be uniquely identified", parser.Identifier())
	}

	tracker[parser.Identifier()] = models.BlockEventParser{
		Identifier:             parser.Identifier(),
		BlockLifecyclePosition: lifecycleValue,
	}
	return registry, tracker, nil
}

func RegisterCustomModels(models []any) {
	indexer.customModels = models
}

func setupIndex(cmd *cobra.Command, args []string) error {
	bindFlags(cmd, viperConf)

	err := indexer.cfg.Validate()
	if err != nil {
		return err
	}

	ignoredKeys := config.CheckSuperfluousIndexKeys(viperConf.AllKeys())

	if len(ignoredKeys) > 0 {
		config.Log.Warnf("Warning, the following invalid keys will be ignored: %v", ignoredKeys)
	}

	setupLogger(indexer.cfg.Log.Level, indexer.cfg.Log.Path, indexer.cfg.Log.Pretty)

	// 0 is an invalid starting block, set it to 1
	if indexer.cfg.Base.StartBlock == 0 {
		indexer.cfg.Base.StartBlock = 1
	}

	db, err := connectToDBAndMigrate(indexer.cfg.Database)
	if err != nil {
		config.Log.Fatal("Could not establish connection to the database", err)
	}

	indexer.db = db

	indexer.dryRun = indexer.cfg.Base.Dry

	indexer.blockEventFilterRegistries = blockEventFilterRegistries{
		beginBlockEventFilterRegistry: &filter.StaticBlockEventFilterRegistry{},
		endBlockEventFilterRegistry:   &filter.StaticBlockEventFilterRegistry{},
	}

	if indexer.cfg.Base.FilterFile != "" {
		f, err := os.Open(indexer.cfg.Base.FilterFile)
		if err != nil {
			config.Log.Fatalf("Failed to open block event filter file %s: %s", indexer.cfg.Base.FilterFile, err)
		}

		b, err := io.ReadAll(f)
		if err != nil {
			config.Log.Fatal("Failed to parse block event filter config", err)
		}

		var fileMessageTypeFilters []filter.MessageTypeFilter

		indexer.blockEventFilterRegistries.beginBlockEventFilterRegistry.BlockEventFilters,
			indexer.blockEventFilterRegistries.beginBlockEventFilterRegistry.RollingWindowEventFilters,
			indexer.blockEventFilterRegistries.endBlockEventFilterRegistry.BlockEventFilters,
			indexer.blockEventFilterRegistries.endBlockEventFilterRegistry.RollingWindowEventFilters,
			fileMessageTypeFilters,
			err = config.ParseJSONFilterConfig(b)

		if err != nil {
			config.Log.Fatal("Failed to parse block event filter config", err)
		}

		indexer.messageTypeFilters = append(indexer.messageTypeFilters, fileMessageTypeFilters...)
	}

	if len(indexer.customModels) != 0 {
		err = dbTypes.MigrateInterfaces(indexer.db, indexer.customModels)
		if err != nil {
			config.Log.Fatal("Failed to migrate custom models", err)
		}
	}

	if len(indexer.customBeginBlockParserTrackers) != 0 {
		err = dbTypes.FindOrCreateCustomBlockEventParsers(indexer.db, indexer.customBeginBlockParserTrackers)
		if err != nil {
			config.Log.Fatal("Failed to migrate custom block event parsers", err)
		}
	}

	if len(indexer.customEndBlockParserTrackers) != 0 {
		err = dbTypes.FindOrCreateCustomBlockEventParsers(indexer.db, indexer.customEndBlockParserTrackers)
		if err != nil {
			config.Log.Fatal("Failed to migrate custom block event parsers", err)
		}
	}

	if len(indexer.customMessageParserTrackers) != 0 {
		err = dbTypes.FindOrCreateCustomMessageParsers(indexer.db, indexer.customMessageParserTrackers)
		if err != nil {
			config.Log.Fatal("Failed to migrate custom message parsers", err)
		}

	}

	return nil
}

// The Indexer struct is used to perform index operations

func setupIndexer() *Indexer {
	var err error

	config.SetChainConfig(indexer.cfg.Probe.AccountPrefix)

	indexer.cl = probe.GetProbeClient(indexer.cfg.Probe, indexer.customModuleBasics)

	// Depending on the app configuration, wait for the chain to catch up
	chainCatchingUp, err := rpc.IsCatchingUp(indexer.cl)
	for indexer.cfg.Base.WaitForChain && chainCatchingUp && err == nil {
		// Wait between status checks, don't spam the node with requests
		config.Log.Debug("Chain is still catching up, please wait or disable check in config.")
		time.Sleep(time.Second * time.Duration(indexer.cfg.Base.WaitForChainDelay))
		chainCatchingUp, err = rpc.IsCatchingUp(indexer.cl)

		// This EOF error pops up from time to time and is unpredictable
		// It is most likely an error on the node, we would need to see any error logs on the node side
		// Try one more time
		if err != nil && strings.HasSuffix(err.Error(), "EOF") {
			time.Sleep(time.Second * time.Duration(indexer.cfg.Base.WaitForChainDelay))
			chainCatchingUp, err = rpc.IsCatchingUp(indexer.cl)
		}
	}
	if err != nil {
		config.Log.Fatal("Error querying chain status.", err)
	}

	return &indexer
}

func index(cmd *cobra.Command, args []string) {
	// Setup the indexer with config, db, and cl
	idxr := setupIndexer()
	dbConn, err := idxr.db.DB()
	if err != nil {
		config.Log.Fatal("Failed to connect to DB", err)
	}
	defer dbConn.Close()

	// blockChans are just the block heights; limit max jobs in the queue, otherwise this queue would contain one
	// item (block height) for every block on the entire blockchain we're indexing. Furthermore, once the queue
	// is close to empty, we will spin up a new thread to fill it up with new jobs.
	blockEnqueueChan := make(chan *core.EnqueueData, 10000)

	// This channel represents query job results for the RPC queries to Cosmos Nodes. Every time an RPC query
	// completes, the query result will be sent to this channel (for later processing by a different thread).
	// Realistically, I expect that RPC queries will be slower than our relational DB on the local network.
	// If RPC queries are faster than DB inserts this buffer will fill up.
	// We will periodically check the buffer size to monitor performance so we can optimize later.
	rpcQueryThreads := int(idxr.cfg.Base.RPCWorkers)
	if rpcQueryThreads == 0 {
		rpcQueryThreads = 4
	} else if rpcQueryThreads > 64 {
		rpcQueryThreads = 64
	}

	var wg sync.WaitGroup // This group is to ensure we are done processing transactions and events before returning

	chain := models.Chain{
		ChainID: idxr.cfg.Probe.ChainID,
		Name:    idxr.cfg.Probe.ChainName,
	}

	dbChainID, err := dbTypes.GetDBChainID(idxr.db, chain)
	if err != nil {
		config.Log.Fatal("Failed to add/create chain in DB", err)
	}

	// This block consolidates all base RPC requests into one worker.
	// Workers read from the enqueued blocks and query blockchain data from the RPC server.
	var blockRPCWaitGroup sync.WaitGroup
	blockRPCWorkerDataChan := make(chan core.IndexerBlockEventData, 10)
	for i := 0; i < rpcQueryThreads; i++ {
		blockRPCWaitGroup.Add(1)
		go core.BlockRPCWorker(&blockRPCWaitGroup, blockEnqueueChan, dbChainID, idxr.cfg.Probe.ChainID, idxr.cfg, idxr.cl, idxr.db, blockRPCWorkerDataChan)
	}

	go func() {
		blockRPCWaitGroup.Wait()
		close(blockRPCWorkerDataChan)
	}()

	// Block BeginBlocker and EndBlocker indexing requirements. Indexes block events that took place in the BeginBlock and EndBlock state transitions
	blockEventsDataChan := make(chan *blockEventsDBData, 4*rpcQueryThreads)
	txDataChan := make(chan *dbData, 4*rpcQueryThreads)

	wg.Add(1)
	go idxr.processBlocks(&wg, core.HandleFailedBlock, blockRPCWorkerDataChan, blockEventsDataChan, txDataChan, dbChainID, indexer.blockEventFilterRegistries)

	wg.Add(1)
	go idxr.doDBUpdates(&wg, txDataChan, blockEventsDataChan, dbChainID)

	switch {
	// If block enqueue function has been explicitly set, use that
	case idxr.blockEnqueueFunction != nil:
	// Default block enqueue functions based on config values
	case idxr.cfg.Base.ReindexMessageType != "":
		idxr.blockEnqueueFunction, err = core.GenerateMsgTypeEnqueueFunction(idxr.db, *idxr.cfg, dbChainID, idxr.cfg.Base.ReindexMessageType)
		if err != nil {
			config.Log.Fatal("Failed to generate block enqueue function", err)
		}
	case idxr.cfg.Base.BlockInputFile != "":
		idxr.blockEnqueueFunction, err = core.GenerateBlockFileEnqueueFunction(idxr.db, *idxr.cfg, idxr.cl, dbChainID, idxr.cfg.Base.BlockInputFile)
		if err != nil {
			config.Log.Fatal("Failed to generate block enqueue function", err)
		}
	default:
		idxr.blockEnqueueFunction, err = core.GenerateDefaultEnqueueFunction(idxr.db, *idxr.cfg, idxr.cl, dbChainID)
		if err != nil {
			config.Log.Fatal("Failed to generate block enqueue function", err)
		}
	}

	err = idxr.blockEnqueueFunction(blockEnqueueChan)
	if err != nil {
		config.Log.Fatal("Block enqueue failed", err)
	}

	close(blockEnqueueChan)

	wg.Wait()
}

type dbData struct {
	txDBWrappers []dbTypes.TxDBWrapper
	block        models.Block
}

type blockEventsDBData struct {
	blockDBWrapper *dbTypes.BlockDBWrapper
}

// This function is responsible for processing raw RPC data into app-usable types. It handles both block events and transactions.
// It parses each dataset according to the application configuration requirements and passes the data to the channels that handle the parsed data.
func (idxr *Indexer) processBlocks(wg *sync.WaitGroup, failedBlockHandler core.FailedBlockHandler, blockRPCWorkerChan chan core.IndexerBlockEventData, blockEventsDataChan chan *blockEventsDBData, txDataChan chan *dbData, chainID uint, blockEventFilterRegistry blockEventFilterRegistries) {
	defer close(blockEventsDataChan)
	defer close(txDataChan)
	defer wg.Done()

	for blockData := range blockRPCWorkerChan {
		currentHeight := blockData.BlockData.Block.Height
		config.Log.Infof("Parsing data for block %d", currentHeight)

		block, err := core.ProcessBlock(blockData.BlockData, blockData.BlockResultsData, chainID)
		if err != nil {
			config.Log.Error("ProcessBlock: unhandled error", err)
			failedBlockHandler(currentHeight, core.UnprocessableTxError, err)
			err := dbTypes.UpsertFailedBlock(idxr.db, currentHeight, idxr.cfg.Probe.ChainID, idxr.cfg.Probe.ChainName)
			if err != nil {
				config.Log.Fatal("Failed to insert failed block", err)
			}
			continue
		}

		if blockData.IndexBlockEvents && !blockData.BlockEventRequestsFailed {
			config.Log.Info("Parsing block events")
			blockDBWrapper, err := core.ProcessRPCBlockResults(*indexer.cfg, block, blockData.BlockResultsData, indexer.customBeginBlockEventParserRegistry, indexer.customEndBlockEventParserRegistry)
			if err != nil {
				config.Log.Errorf("Failed to process block events during block %d event processing, adding to failed block events table", currentHeight)
				failedBlockHandler(currentHeight, core.FailedBlockEventHandling, err)
				err := dbTypes.UpsertFailedEventBlock(idxr.db, currentHeight, idxr.cfg.Probe.ChainID, idxr.cfg.Probe.ChainName)
				if err != nil {
					config.Log.Fatal("Failed to insert failed block event", err)
				}
			} else {
				config.Log.Infof("Finished parsing block event data for block %d", currentHeight)

				var beginBlockFilterError error
				var endBlockFilterError error
				if blockEventFilterRegistry.beginBlockEventFilterRegistry != nil && blockEventFilterRegistry.beginBlockEventFilterRegistry.NumFilters() > 0 {
					blockDBWrapper.BeginBlockEvents, beginBlockFilterError = core.FilterRPCBlockEvents(blockDBWrapper.BeginBlockEvents, *blockEventFilterRegistry.beginBlockEventFilterRegistry)
				}

				if blockEventFilterRegistry.endBlockEventFilterRegistry != nil && blockEventFilterRegistry.endBlockEventFilterRegistry.NumFilters() > 0 {
					blockDBWrapper.EndBlockEvents, endBlockFilterError = core.FilterRPCBlockEvents(blockDBWrapper.EndBlockEvents, *blockEventFilterRegistry.endBlockEventFilterRegistry)
				}

				if beginBlockFilterError == nil && endBlockFilterError == nil {
					blockEventsDataChan <- &blockEventsDBData{
						blockDBWrapper: blockDBWrapper,
					}
				} else {
					config.Log.Errorf("Failed to filter block events during block %d event processing, adding to failed block events table. Begin blocker filter error %s. End blocker filter error %s", currentHeight, beginBlockFilterError, endBlockFilterError)
					failedBlockHandler(currentHeight, core.FailedBlockEventHandling, err)
					err := dbTypes.UpsertFailedEventBlock(idxr.db, currentHeight, idxr.cfg.Probe.ChainID, idxr.cfg.Probe.ChainName)
					if err != nil {
						config.Log.Fatal("Failed to insert failed block event", err)
					}
				}
			}
		}

		if blockData.IndexTransactions && !blockData.TxRequestsFailed {
			config.Log.Info("Parsing transactions")
			var txDBWrappers []dbTypes.TxDBWrapper
			var err error

			if blockData.GetTxsResponse != nil {
				config.Log.Debug("Processing TXs from RPC TX Search response")
				txDBWrappers, _, err = core.ProcessRPCTXs(idxr.cfg, idxr.db, idxr.cl, idxr.messageTypeFilters, blockData.GetTxsResponse, indexer.customMessageParserRegistry)
			} else if blockData.BlockResultsData != nil {
				config.Log.Debug("Processing TXs from BlockResults search response")
				txDBWrappers, _, err = core.ProcessRPCBlockByHeightTXs(idxr.cfg, idxr.db, idxr.cl, idxr.messageTypeFilters, blockData.BlockData, blockData.BlockResultsData, indexer.customMessageParserRegistry)
			}

			if err != nil {
				config.Log.Error("ProcessRpcTxs: unhandled error", err)
				failedBlockHandler(currentHeight, core.UnprocessableTxError, err)
				err := dbTypes.UpsertFailedBlock(idxr.db, currentHeight, idxr.cfg.Probe.ChainID, idxr.cfg.Probe.ChainName)
				if err != nil {
					config.Log.Fatal("Failed to insert failed block", err)
				}
			} else {
				txDataChan <- &dbData{
					txDBWrappers: txDBWrappers,
					block:        block,
				}
			}

		}
	}
}

// doDBUpdates will read the data out of the db data chan that had been processed by the workers
// if this is a dry run, we will simply empty the channel and track progress
// otherwise we will index the data in the DB.
// it will also read rewars data and index that.
func (idxr *Indexer) doDBUpdates(wg *sync.WaitGroup, txDataChan chan *dbData, blockEventsDataChan chan *blockEventsDBData, dbChainID uint) {
	blocksProcessed := 0
	dbWrites := 0
	dbReattempts := 0
	timeStart := time.Now()
	defer wg.Done()

	for {
		// break out of loop once all channels are fully consumed
		if txDataChan == nil && blockEventsDataChan == nil {
			config.Log.Info("DB updates complete")
			break
		}

		select {
		// read tx data from the data chan
		case data, ok := <-txDataChan:
			if !ok {
				txDataChan = nil
				continue
			}
			dbWrites++
			// While debugging we'll sometimes want to turn off INSERTS to the DB
			// Note that this does not turn off certain reads or DB connections.
			if !idxr.dryRun {
				config.Log.Info(fmt.Sprintf("Indexing %v TXs from block %d", len(data.txDBWrappers), data.block.Height))
				_, indexedDataset, err := dbTypes.IndexNewBlock(idxr.db, data.block, data.txDBWrappers, *idxr.cfg)
				if err != nil {
					// Do a single reattempt on failure
					dbReattempts++
					_, _, err = dbTypes.IndexNewBlock(idxr.db, data.block, data.txDBWrappers, *idxr.cfg)
					if err != nil {
						config.Log.Fatal(fmt.Sprintf("Error indexing block %v.", data.block.Height), err)
					}
				}

				err = dbTypes.IndexCustomMessages(*idxr.cfg, idxr.db, idxr.dryRun, indexedDataset, idxr.customMessageParserTrackers)

				if err != nil {
					config.Log.Fatal(fmt.Sprintf("Error indexing custom messages for block %d", data.block.Height), err)
				}

				config.Log.Info(fmt.Sprintf("Finished indexing %v TXs from block %d", len(data.txDBWrappers), data.block.Height))
			} else {
				config.Log.Info(fmt.Sprintf("Processing block %d (dry run, block data will not be stored in DB).", data.block.Height))
			}

			// Just measuring how many blocks/second we can process
			if idxr.cfg.Base.BlockTimer > 0 {
				blocksProcessed++
				if blocksProcessed%int(idxr.cfg.Base.BlockTimer) == 0 {
					totalTime := time.Since(timeStart)
					config.Log.Info(fmt.Sprintf("Processing %d blocks took %f seconds. %d total blocks have been processed.\n", idxr.cfg.Base.BlockTimer, totalTime.Seconds(), blocksProcessed))
					timeStart = time.Now()
				}
				if float64(dbReattempts)/float64(dbWrites) > .1 {
					config.Log.Fatalf("More than 10%% of the last %v DB writes have failed.", dbWrites)
				}
			}
		case eventData, ok := <-blockEventsDataChan:
			if !ok {
				blockEventsDataChan = nil
				continue
			}
			dbWrites++
			numEvents := len(eventData.blockDBWrapper.BeginBlockEvents) + len(eventData.blockDBWrapper.EndBlockEvents)
			config.Log.Info(fmt.Sprintf("Indexing %v Block Events from block %d", numEvents, eventData.blockDBWrapper.Block.Height))
			identifierLoggingString := fmt.Sprintf("block %d", eventData.blockDBWrapper.Block.Height)

			indexedDataset, err := dbTypes.IndexBlockEvents(idxr.db, idxr.dryRun, eventData.blockDBWrapper, identifierLoggingString)
			if err != nil {
				config.Log.Fatal(fmt.Sprintf("Error indexing block events for %s.", identifierLoggingString), err)
			}

			err = dbTypes.IndexCustomBlockEvents(*idxr.cfg, idxr.db, idxr.dryRun, indexedDataset, identifierLoggingString, idxr.customBeginBlockParserTrackers, idxr.customEndBlockParserTrackers)

			if err != nil {
				config.Log.Fatal(fmt.Sprintf("Error indexing custom block events for %s.", identifierLoggingString), err)
			}

			config.Log.Info(fmt.Sprintf("Finished indexing %v Block Events from block %d", numEvents, eventData.blockDBWrapper.Block.Height))
		}
	}
}
