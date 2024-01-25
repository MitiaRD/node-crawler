package main

import (
	"database/sql"
	"fmt"
	"github.com/ethereum/go-ethereum/cmd/utils"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/node-crawler/pkg/crawler"
	"os"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/node-crawler/pkg/api"
	"github.com/ethereum/node-crawler/pkg/apidb"
	"github.com/ethereum/node-crawler/pkg/crawlerdb"
	"github.com/urfave/cli/v2"
)

var (
	apiCommand = &cli.Command{
		Name:   "api",
		Usage:  "API server for the crawler",
		Action: startAPI,
		Flags: []cli.Flag{
			apiDBFlag,
			apiListenAddrFlag,
			autovacuumFlag,
			busyTimeoutFlag,
			crawlerDBFlag,
			dropNodesTimeFlag,
			listenAddrFlag,
			bootnodesFlag,
			geoipdbFlag,
			nodeFileFlag,
			nodeURLFlag,
			nodedbFlag,
			nodekeyFlag,
			timeoutFlag,
			workersFlag,
			utils.GoerliFlag,
			utils.NetworkIdFlag,
			utils.SepoliaFlag,
		},
	}
)

func startAPI(ctx *cli.Context) error {
	var (
		crawlerDBPath        = ctx.String(crawlerDBFlag.Name)
		apiDBPath            = ctx.String(apiDBFlag.Name)
		autovacuum           = ctx.String(autovacuumFlag.Name)
		busyTimeout          = ctx.Uint64(busyTimeoutFlag.Name)
		crawlerListeningAddr = ctx.String(listenAddrFlag.Name)
	)

	crawlerDB, err := openSQLiteDB(
		crawlerDBPath,
		autovacuum,
		busyTimeout,
	)
	if err != nil {
		return err
	}

	shouldInit := false
	if _, err := os.Stat(apiDBPath); os.IsNotExist(err) {
		shouldInit = true
	}
	nodeDB, err := openSQLiteDB(
		apiDBPath,
		autovacuum,
		busyTimeout,
	)
	if err != nil {
		return err
	}
	if shouldInit {
		log.Info("DB did not exist, init")
		if err := apidb.CreateDB(nodeDB); err != nil {
			return err
		}
	}

	enodeDB, err := enode.OpenDB(ctx.String(nodedbFlag.Name))
	if err != nil {
		panic(err)
	}

	crawler := crawler.Crawler{
		NetworkID:  ctx.Uint64(utils.NetworkIdFlag.Name),
		NodeURL:    ctx.String(nodeURLFlag.Name),
		ListenAddr: crawlerListeningAddr,
		NodeKey:    ctx.String(nodekeyFlag.Name),
		Bootnodes:  ctx.StringSlice(bootnodesFlag.Name),
		Timeout:    ctx.Duration(timeoutFlag.Name),
		Workers:    ctx.Uint64(workersFlag.Name),
		Sepolia:    ctx.Bool(utils.SepoliaFlag.Name),
		Goerli:     ctx.Bool(utils.GoerliFlag.Name),
		NodeDB:     enodeDB,
		CrawlerDB:  crawlerDB,
	}
	log.Info(fmt.Sprintf("1. crawler listen address: %v : %v", crawler.ListenAddr, ctx.String(listenAddrFlag.Name)))

	// Start daemons
	var wg sync.WaitGroup
	wg.Add(3)

	// Start reading daemon
	go func() {
		defer wg.Done()
		newNodeDaemon(crawlerDB, nodeDB)
	}()

	// Start the drop daemon
	go func() {
		defer wg.Done()
		dropDaemon(nodeDB, ctx.Duration(dropNodesTimeFlag.Name))
	}()

	// Start the API deamon
	apiAddress := ctx.String(apiListenAddrFlag.Name)
	apiDaemon := api.New(apiAddress, nodeDB, crawler)
	go func() {
		defer wg.Done()
		log.Info(fmt.Sprintf("2. crawler listen address: %v", &apiDaemon.Crawler.ListenAddr))
		apiDaemon.HandleRequests()
	}()
	wg.Wait()

	return nil
}

func transferNewNodes(crawlerDB, nodeDB *sql.DB) error {
	crawlerDBTx, err := crawlerDB.Begin()
	if err != nil {
		// Sometimes error occur trying to read the crawler database, but
		// they are normally recoverable, and a lot of the time, it's
		// because the database is locked by the crawler.
		return fmt.Errorf("error starting transaction to read nodes: %w", err)
	}
	defer crawlerDBTx.Rollback()

	nodes, err := crawlerdb.ReadAndDeleteUnseenNodes(crawlerDBTx)
	if err != nil {
		// Simiar to nodeDB.Begin() error
		return fmt.Errorf("error reading nodes: %w", err)
	}

	if len(nodes) > 0 {
		err := apidb.InsertCrawledNodes(nodeDB, nodes)
		if err != nil {
			// This shouldn't happen because the database is not shared in this
			// instance, so there shouldn't be lock errors, but anything can
			// happen. We will still try again.
			return fmt.Errorf("error inserting nodes: %w", err)
		}
		log.Info("Nodes inserted", "len", len(nodes))
	}

	crawlerDBTx.Commit()
	return nil
}

// newNodeDaemon reads new nodes from the crawler and puts them in the db
// Might trigger the invalidation of caches for the api in the future
func newNodeDaemon(crawlerDB, nodeDB *sql.DB) {
	// Exponentially increase the backoff time
	retryTimeout := time.Minute

	for {
		err := transferNewNodes(crawlerDB, nodeDB)
		if err != nil {
			log.Error("Failure in transferring new nodes", "err", err)
			time.Sleep(retryTimeout)
			retryTimeout *= 2
			continue
		}

		retryTimeout = time.Minute
		time.Sleep(time.Second)
	}
}

func dropDaemon(db *sql.DB, dropTimeout time.Duration) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for {
		<-ticker.C
		err := apidb.DropOldNodes(db, dropTimeout)
		if err != nil {
			panic(err)
		}
	}
}
