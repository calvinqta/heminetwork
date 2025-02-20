// Copyright (c) 2024-2025 Hemi Labs, Inc.
// Use of this source code is governed by the MIT License,
// which can be found in the LICENSE file.

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/juju/loggo"
	"github.com/mitchellh/go-homedir"

	"github.com/hemilabs/heminetwork/api/bfgapi"
	"github.com/hemilabs/heminetwork/api/bssapi"
	"github.com/hemilabs/heminetwork/api/protocol"
	"github.com/hemilabs/heminetwork/api/tbcapi"
	"github.com/hemilabs/heminetwork/config"
	"github.com/hemilabs/heminetwork/database/bfgd/postgres"
	"github.com/hemilabs/heminetwork/database/tbcd"
	"github.com/hemilabs/heminetwork/database/tbcd/level"
	"github.com/hemilabs/heminetwork/service/tbc"
	"github.com/hemilabs/heminetwork/service/tbc/peer"
	"github.com/hemilabs/heminetwork/version"
)

const (
	daemonName      = "hemictl"
	defaultLogLevel = daemonName + "=INFO;bfgpostgres=INFO;postgres=INFO;protocol=INFO"

	tbcReadLimit = 8 * (1 << 20) // 8 MiB.
)

var (
	log     = loggo.GetLogger(daemonName)
	welcome string

	bssURL      string
	logLevel    string
	leveldbHome string
	network     string
	cm          = config.CfgMap{
		"HEMICTL_LEVELDB_HOME": config.Config{
			Value:        &leveldbHome,
			DefaultValue: "~/.tbcd",
			Help:         "leveldb home directory",
			Print:        config.PrintAll,
		},
		"HEMICTL_NETWORK": config.Config{
			Value:        &network,
			DefaultValue: "mainnet",
			Help:         "hemictl network",
			Print:        config.PrintAll,
		},
		"HEMICTL_BSS_URL": config.Config{
			Value:        &bssURL,
			DefaultValue: bssapi.DefaultURL,
			Help:         "BSS websocket server host and route",
			Print:        config.PrintAll,
		},
		"HEMICTL_LOG_LEVEL": config.Config{
			Value:        &logLevel,
			DefaultValue: defaultLogLevel,
			Help:         "loglevel for various packages; INFO, DEBUG and TRACE",
			Print:        config.PrintAll,
		},
	}

	callTimeout = 100 * time.Second
)

// handleBSSWebsocketReadUnauth discards all reads but has to exist in order to
// be able to use bssapi.Call.
func handleBSSWebsocketReadUnauth(ctx context.Context, conn *protocol.Conn) {
	for {
		if _, _, _, err := bssapi.ReadConn(ctx, conn); err != nil {
			return
		}
	}
}

// handleBSSWebsocketReadUnauth discards all reads but has to exist in order to
// be able to use bfgapi.Call.
func handleBFGWebsocketReadUnauth(ctx context.Context, conn *protocol.Conn) {
	for {
		if _, _, _, err := bfgapi.ReadConn(ctx, conn); err != nil {
			return
		}
	}
}

// handleTBCWebsocketRead discards all reads but has to exist in order to
// be able to use tbcapi.Call.
func handleTBCWebsocketRead(ctx context.Context, conn *protocol.Conn) {
	for {
		if _, _, _, err := tbcapi.ReadConn(ctx, conn); err != nil {
			return
		}
	}
}

func bfgdb() error {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()

	pgURI := os.Getenv("PGURI") // XXX mpve into config
	if pgURI == "" {
		// construct pgURI based on reasonable defaults.
		home, err := homedir.Dir()
		if err != nil {
			return fmt.Errorf("dir: %w", err)
		}
		user, err := user.Current()
		if err != nil {
			return fmt.Errorf("current: %w", err)
		}

		filename := filepath.Join(home, ".pgsql-bfgdb-"+user.Username)
		password, err := os.ReadFile(filename)
		if err != nil {
			return fmt.Errorf("read file: %w", err)
		}
		pgURI = fmt.Sprintf("database=bfgdb password=%s", password)
	}

	db, err := postgres.New(ctx, pgURI)
	if err != nil {
		return fmt.Errorf("new: %w", err)
	}
	defer db.Close()

	param := flag.Arg(2)
	c := flag.Arg(1)
	out := make(map[string]any, 10)
	switch c {
	case "version":
		out["bfgdb_version"], err = db.Version(ctx)
		if err != nil {
			return fmt.Errorf("error received getting version: %s", err.Error())
		}
	default:
		return fmt.Errorf("invalid bfgdb command: %v", c)
	}
	_ = param

	o, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	fmt.Printf("%s\n", o)

	return nil
}

func parseArgs(args []string) (string, map[string]string, error) {
	if len(args) < 1 {
		flag.Usage()
		return "", nil, errors.New("action required")
	}

	action := args[0]
	parsed := make(map[string]string, 10)

	for _, v := range args[1:] {
		s := strings.Split(v, "=")
		if len(s) != 2 {
			return "", nil, fmt.Errorf("invalid argument: %v", v)
		}
		if len(s[0]) == 0 || len(s[1]) == 0 {
			return "", nil, fmt.Errorf("expected a=b, got %v", v)
		}
		parsed[s[0]] = s[1]
	}

	return action, parsed, nil
}

func tbcdb(pctx context.Context) error {
	ctx, cancel := context.WithCancel(pctx)
	defer cancel()

	action, args, err := parseArgs(flag.Args()[1:])
	if err != nil {
		return err
	}

	level.Welcome = false
	tbc.Welcome = false
	cfg := tbc.NewDefaultConfig()
	cfg.LevelDBHome = leveldbHome
	cfg.Network = network
	cfg.PeersWanted = 0    // disable peer manager
	cfg.ListenAddress = "" // disable RPC
	s, err := tbc.NewServer(cfg)
	if err != nil {
		return fmt.Errorf("new server: %w", err)
	}
	go func() {
		if err := s.Run(ctx); err != nil {
			panic(fmt.Errorf("run server: %w", err))
		}
	}()
	for !s.Running() {
		time.Sleep(time.Millisecond)
	}

	// commands
	switch action {
	case "blockheaderbyhash":
		hash := args["hash"]
		if hash == "" {
			return errors.New("hash: must be set")
		}
		ch, err := chainhash.NewHashFromStr(hash)
		if err != nil {
			return fmt.Errorf("chainhash: %w", err)
		}
		bh, height, err := s.BlockHeaderByHash(ctx, ch)
		if err != nil {
			return fmt.Errorf("block header by hash: %w", err)
		}
		fmt.Printf("hash  : %v\n", bh)
		fmt.Printf("height: %v\n", height)

	case "blockheaderbest":
		height, bh, err := s.BlockHeaderBest(ctx)
		if err != nil {
			return fmt.Errorf("block header best: %w", err)
		}
		fmt.Printf("hash  : %v\n", bh.BlockHash())
		fmt.Printf("height: %v\n", height)

	case "blockheadersbyheight":
		height := args["height"]
		if height == "" {
			return errors.New("height: must be set")
		}
		h, err := strconv.ParseUint(height, 10, 64)
		if err != nil {
			return fmt.Errorf("parse uint: %w", err)
		}
		bh, err := s.BlockHeadersByHeight(ctx, h)
		if err != nil {
			return fmt.Errorf("block header by height: %w", err)
		}
		spew.Dump(bh)

	// case "blockheadersinsert":

	case "blocksmissing":
		count := args["count"]
		c, err := strconv.ParseInt(count, 10, 32)
		if len(count) > 0 && err != nil {
			return fmt.Errorf("parse uint: %w", err)
		}
		bi, err := s.BlocksMissing(ctx, int(c))
		if err != nil {
			return fmt.Errorf("blocks missing: %w", err)
		}
		for k := range bi {
			fmt.Printf("%v: %v\n", bi[k].Height, bi[k].Hash)
		}

	// case "blockinsert":

	case "blockbyhash":
		hash := args["hash"]
		if hash == "" {
			return errors.New("hash: must be set")
		}
		ch, err := chainhash.NewHashFromStr(hash)
		if err != nil {
			return fmt.Errorf("chainhash: %w", err)
		}
		b, err := s.BlockByHash(ctx, ch)
		if err != nil {
			return fmt.Errorf("block by hash: %w", err)
		}
		spew.Dump(b)

	case "feesbyheight":
		height := args["height"]
		if height == "" {
			return errors.New("height: must be set")
		}
		h, err := strconv.ParseInt(height, 10, 64)
		if err != nil {
			return fmt.Errorf("parse uint: %w", err)
		}
		count := args["count"]
		c, err := strconv.ParseInt(count, 10, 64)
		if len(count) > 0 && err != nil {
			return fmt.Errorf("parse uint: %w", err)
		}
		if c == 0 {
			c = 1
		}
		bh, err := s.FeesAtHeight(ctx, h, c)
		if err != nil {
			return fmt.Errorf("fees by height: %w", err)
		}
		spew.Dump(bh)

	case "help", "h":
		fmt.Println("tbcd db manipulator commands:")
		fmt.Println("\tbalancebyscripthash [hash]")
		fmt.Println("\tblockbyhash [hash]")
		fmt.Println("\tblockheaderbyhash [hash]")
		fmt.Println("\tblockheaderbest")
		fmt.Println("\tblockheadersbyheight [height]")
		fmt.Println("\tblocksbytxid [hash]")
		fmt.Println("\tblocksmissing [count]")
		fmt.Println("\tdeletemetadata")
		fmt.Println("\tdumpmetadata")
		fmt.Println("\tdumpoutputs <prefix>")
		fmt.Println("\thelp")
		fmt.Println("\tscripthashbyoutpoint [txid] [index]")
		fmt.Println("\tspentoutputsbytxid <txid>")
		fmt.Println("\ttxbyid <hash>")
		fmt.Println("\ttxindex <height> <count> <maxcache>")
		fmt.Println("\tutxoindex <height> <count> <maxcache>")
		fmt.Println("\tutxosbyscripthash [hash]")
		fmt.Println("\tversion")

	case "utxoindex":
		hash := args["hash"]
		if hash == "" {
			return errors.New("must provide hash")
		}
		eh, err := chainhash.NewHashFromStr(hash)
		if err != nil {
			return fmt.Errorf("parse hash: %w", err)
		}

		maxCache := args["maxcache"]
		var mc uint64
		if maxCache != "" {
			if mc, err = strconv.ParseUint(maxCache, 10, 64); err != nil {
				return fmt.Errorf("maxCache: %w", err)
			}
			cfg.MaxCachedTxs = int(mc)
		}
		err = s.UtxoIndexer(ctx, eh)
		if err != nil {
			return fmt.Errorf("indexer: %w", err)
		}

	case "txindex":
		hash := args["hash"]
		if hash == "" {
			return errors.New("must provide hash")
		}
		eh, err := chainhash.NewHashFromStr(hash)
		if err != nil {
			return fmt.Errorf("parse hash: %w", err)
		}

		maxCache := args["maxcache"]
		var mc uint64
		if maxCache != "" {
			if mc, err = strconv.ParseUint(maxCache, 10, 64); err != nil {
				return fmt.Errorf("maxCache: %w", err)
			}
			cfg.MaxCachedTxs = int(mc)
		}
		if err = s.TxIndexer(ctx, eh); err != nil {
			return fmt.Errorf("indexer: %w", err)
		}

	case "blockhashbytxid":
		txid := args["txid"]
		if txid == "" {
			return errors.New("txid: must be set")
		}
		chtxid, err := chainhash.NewHashFromStr(txid)
		if err != nil {
			return fmt.Errorf("chainhash: %w", err)
		}

		bh, err := s.BlockHashByTxId(ctx, chtxid)
		if err != nil {
			return fmt.Errorf("block by txid: %w", err)
		}
		fmt.Printf("%v\n", bh)

	case "txbyid":
		txid := args["txid"]
		if txid == "" {
			return errors.New("txid: must be set")
		}
		chtxid, err := chainhash.NewHashFromStr(txid)
		if err != nil {
			return fmt.Errorf("chainhash: %w", err)
		}

		tx, err := s.TxById(ctx, chtxid)
		if err != nil {
			return fmt.Errorf("block by txid: %w", err)
		}
		fmt.Printf("%v\n", spew.Sdump(tx))

	case "spentoutputsbytxid":
		txid := args["txid"]
		if txid == "" {
			return errors.New("txid: must be set")
		}
		chtxid, err := chainhash.NewHashFromStr(txid)
		if err != nil {
			return fmt.Errorf("chainhash: %w", err)
		}

		si, err := s.SpentOutputsByTxId(ctx, chtxid)
		if err != nil {
			return fmt.Errorf("spend outputs by txid: %w", err)
		}
		for k := range si {
			fmt.Printf("%v\n", si[k])
		}

	case "scripthashbyoutpoint":
		// XXX this does not call ScriptHashByOutpoint FIXME
		txid := args["txid"]
		if txid == "" {
			return errors.New("txid: must be set")
		}
		chtxid, err := chainhash.NewHashFromStr(txid)
		if err != nil {
			return fmt.Errorf("chainhash: %w", err)
		}

		si, err := s.SpentOutputsByTxId(ctx, chtxid)
		if err != nil {
			return fmt.Errorf("spend outputs by txid: %w", err)
		}
		for k := range si {
			fmt.Printf("%v\n", si[k])
		}

	case "blockintxindex":
		blkid := args["blkid"]
		if blkid == "" {
			return errors.New("blkid: must be set")
		}
		blkhash, err := chainhash.NewHashFromStr(blkid)
		if err != nil {
			return fmt.Errorf("chainhash: %w", err)
		}
		ok, err := s.BlockInTxIndex(ctx, blkhash)
		if err != nil {
			return fmt.Errorf("block in transaction index: %w", err)
		}
		fmt.Printf("%v\n", ok)

	case "balancebyscripthash":
		address := args["address"]
		hash := args["hash"]
		if address == "" && hash == "" {
			return errors.New("hash or address: must be set")
		} else if address != "" && hash != "" {
			return errors.New("hash or address: both set")
		}

		var sh tbcd.ScriptHash
		if hash != "" {
			sh, err = tbcd.NewScriptHashFromString(hash)
			if err != nil {
				return fmt.Errorf("new scripthash from string: %w", err)
			}
		}
		if address != "" {
			// XXX set params
			a, err := btcutil.DecodeAddress(address, &chaincfg.TestNet3Params)
			if err != nil {
				return err
			}
			h, err := txscript.PayToAddrScript(a)
			if err != nil {
				return err
			}
			sh = tbcd.NewScriptHashFromScript(h)
		}

		balance, err := s.BalanceByScriptHash(ctx, sh)
		if err != nil {
			return fmt.Errorf("block by hash: %w", err)
		}
		spew.Dump(balance)

	case "utxosbyscripthash":
		address := args["address"]
		hash := args["hash"]
		count := args["count"]
		start := args["start"]

		if address == "" && hash == "" {
			return errors.New("hash or address: must be set")
		} else if address != "" && hash != "" {
			return errors.New("hash or address: both set")
		}

		if count == "" {
			count = "100"
		}

		if start == "" {
			start = "0"
		}

		countNum, err := strconv.ParseUint(count, 10, 64)
		if err != nil {
			return err
		}

		startNum, err := strconv.ParseUint(start, 10, 64)
		if err != nil {
			return err
		}

		var sh tbcd.ScriptHash
		if hash != "" {
			sh, err = tbcd.NewScriptHashFromString(hash)
			if err != nil {
				return err
			}
		}
		if address != "" {
			// XXX set params
			a, err := btcutil.DecodeAddress(address, &chaincfg.TestNet3Params)
			if err != nil {
				return err
			}
			h, err := txscript.PayToAddrScript(a)
			if err != nil {
				return err
			}
			sh = tbcd.NewScriptHashFromScript(h)
		}

		utxos, err := s.UtxosByScriptHash(ctx, sh, startNum, countNum)
		if err != nil {
			return fmt.Errorf("block by hash: %w", err)
		}
		var balance uint64
		for k := range utxos {
			fmt.Printf("%v\n", utxos[k])
			balance += utxos[k].Value()
		}
		fmt.Printf("utxos: %v total: %v\n", len(utxos), balance)

	// XXX this needs to be hidden behind a debug flug of sorts.
	// case "dbget":
	//	dbname := args["dbname"]
	//	if dbname == "" {
	//		return errors.New("dbname: must be set")
	//	}

	//	key := args["key"]
	//	if key == "" {
	//		return errors.New("key: must be set")
	//	}

	//	value, err := s.DatabaseGet(ctx, dbname, []byte(key))
	//	if err != nil {
	//		return fmt.Errorf("metadata get: %w", err)
	//	}
	//	spew.Dump(value)

	// XXX this needs to be hidden behind a debug flug of sorts.
	case "metadatadel":
		key := args["key"]
		if key == "" {
			return errors.New("key: must be set")
		}

		return fmt.Errorf("fixme deletemetadata")

	// XXX this needs to be hidden behind a debug flug of sorts.
	case "metadataget":
		key := args["key"]
		if key == "" {
			return errors.New("key: must be set")
		}

		value, err := s.DatabaseMetadataGet(ctx, []byte(key))
		if err != nil {
			return fmt.Errorf("metadata get: %w", err)
		}
		spew.Dump(value)
	case "dumpmetadata":
		return fmt.Errorf("fixme dumpmetadata")

	case "dumpoutputs":
		return fmt.Errorf("fixme dumpoutputs")
		// s.DBClose()

		//levelDBHome := "~/.tbcd" // XXX
		//network := "testnet3"
		//db, err := level.New(ctx, level.NewConfig(filepath.Join(levelDBHome, network), "1mb", "128mb"))
		//if err != nil {
		//	return err
		//}
		//defer db.Close()
		//prefix := args["prefix"]
		//if len(prefix) > 1 {
		//	return errors.New("prefix must be one byte")
		//} else if len(prefix) == 1 && !(prefix[0] == 'h' || prefix[0] == 'u') {
		//	return errors.New("prefix must be h or u")
		//}
		//pool := db.DB()
		//outsDB := pool[ldb.OutputsDB]
		//it := outsDB.NewIterator(&util.Range{Start: []byte(prefix)}, nil)
		//defer it.Release()
		//for it.Next() {
		//	fmt.Printf("outputs key %vvalue %v", spew.Sdump(it.Key()), spew.Sdump(it.Value()))
		//}

	case "version":
		version, err := s.DatabaseVersion(ctx)
		if err != nil {
			return fmt.Errorf("version: %w", err)
		}
		fmt.Printf("database version: %v\n", version)

	case "metadatabatchget", "metadatabatchput", "blockheadergenesisinsert",
		"blockheadercachestats", "blockheadersinsert", "blockheadersremove",
		"blockmissingdelete", "blockinsert", "blockcachestats",
		"blockutxoupdate", "BlockTxUpdate", "blockkeystoneupdate":
		fmt.Printf("not yet: %v", action)

	// XXX implement ASAP
	case "metadataput", "blockheaderbyutxoindex",
		"blockheaderbytxindex", "utxosbyscripthashcount",
		"blockkeystonebyl2keystoneabrevhash", "blockheaderbykeystoneindex",
		"dbdel", "dbget", "dbput" /* these three are syntetic */ :
		fmt.Printf("not yet: %v", action)

	default:
		return fmt.Errorf("invalid action: %v", action)
	}

	return nil
}

func p2p() error {
	action, args, err := parseArgs(flag.Args()[1:])
	if err != nil {
		return err
	}

	if action == "help" || action == "h" {
		fmt.Println("p2p actions:")
		fmt.Println("\tfeefilter                      - returns advertised fee filter")
		fmt.Println("\tgetaddr                        - retrieve p2p information")
		fmt.Println("\tgetblock [hash]                - this is a compounded command, returns a block")
		fmt.Println("\tgetdata [hash] [type=tx|block] - returns a tx or block")
		fmt.Println("\tgetheaders [hash]              - returns up to 2000 headers from provided hash")
		fmt.Println("\tgettx [hash]                   - retrieve mempool tx")
		fmt.Println("\tmempool                        - retrieve mempool from peer, slow and not always enabled")
		fmt.Println("\tping <nonce>                   - ping remote node with a nonce")
		fmt.Println("\tremote                         - return remote version")
		fmt.Println("")
		fmt.Println("All actions support [addr=netaddress] <out=[json|raw|spew]> <net=[mainnet|testnet|testnet3]> <timeout=duration>")
		fmt.Println("")
		fmt.Println("Example: hemictl p2p ping addr=127.0.0.1:18333 nonce=1337 out=json")

		return nil
	}

	timeout := 30 * time.Second
	to := args["timeout"]
	if to != "" {
		timeout, err = time.ParseDuration(to)
		if err != nil {
			return fmt.Errorf("timeout: %w", err)
		}
	}

	addr := args["addr"]
	if addr == "" {
		return fmt.Errorf("addr required")
	}

	var network wire.BitcoinNet
	net := args["net"]
	switch net {
	case "mainnet":
		network = wire.MainNet
	case "testnet":
		network = wire.TestNet
	case "", "testnet3":
		network = wire.TestNet3
	default:
		return fmt.Errorf("invalid net: %v", net)
	}

	cp, err := peer.New(network, 0xc0ffee, addr)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if err = cp.Connect(ctx); err != nil {
		return err
	}
	defer cp.Close() // XXX: handle error?

	// commands
	var msg wire.Message
	switch action {
	case "feefilter":
		// loop here for a bit since fee filter shows up late
		for i := 0; i < 10; i++ {
			time.Sleep(100 * time.Millisecond)
			msg, err = cp.FeeFilter()
			if err != nil {
				continue
			}
		}
		if msg == nil {
			return fmt.Errorf("fee filter: %w", err)
		}

	case "getaddr":
		a, err := cp.GetAddr(ctx)
		if err != nil {
			return fmt.Errorf("get addr: %w", err)
		}
		switch m := a.(type) {
		case *wire.MsgAddr:
			msg = m
		case *wire.MsgAddrV2:
			msg = m
		default:
			return fmt.Errorf("invalid get addr type: %T", a)
		}

	case "getblock":
		hash := args["hash"]
		if hash == "" {
			return errors.New("hash: must be set")
		}

		ch, err := chainhash.NewHashFromStr(hash)
		if err != nil {
			return fmt.Errorf("chainhash: %w", err)
		}
		msg, err = cp.GetBlock(ctx, ch)
		if err != nil {
			return fmt.Errorf("get block: %w", err)
		}

	case "getdata":
		var typ wire.InvType
		ty := args["type"]
		switch ty {
		case "tx":
			typ = wire.InvTypeTx
		case "block":
			typ = wire.InvTypeBlock
		default:
			return fmt.Errorf("invalid type: %v", ty)
		}
		hash := args["hash"]
		if hash == "" {
			return errors.New("hash: must be set")
		}

		ch, err := chainhash.NewHashFromStr(hash)
		if err != nil {
			return fmt.Errorf("chainhash: %w", err)
		}
		gd, err := cp.GetData(ctx, wire.NewInvVect(typ, ch))
		if err != nil {
			return fmt.Errorf("get data: %w", err)
		}
		switch m := gd.(type) {
		case *wire.MsgBlock:
			msg = m
		case *wire.MsgTx:
			msg = m
		case *wire.MsgNotFound:
			// note that json will look like a successful not found error
			msg = m
		}

	case "getheaders":
		hash := args["hash"]
		if hash == "" {
			return errors.New("hash: must be set")
		}

		ch, err := chainhash.NewHashFromStr(hash)
		if err != nil {
			return fmt.Errorf("chainhash: %w", err)
		}
		msg, err = cp.GetHeaders(ctx, []*chainhash.Hash{ch}, nil)
		if err != nil {
			return fmt.Errorf("get headers: %w", err)
		}

	case "gettx":
		hash := args["hash"]
		if hash == "" {
			return errors.New("hash: must be set")
		}

		ch, err := chainhash.NewHashFromStr(hash)
		if err != nil {
			return fmt.Errorf("chainhash: %w", err)
		}
		msg, err = cp.GetTx(ctx, ch)
		if err != nil {
			return fmt.Errorf("get tx: %w", err)
		}

	case "mempool":
		msg, err = cp.MemPool(ctx)
		if err != nil {
			return fmt.Errorf("mempool: %w", err)
		}

	case "ping":
		nonce := args["nonce"]
		n := uint64(0)
		if nonce != "" {
			n, err = strconv.ParseUint(nonce, 10, 64)
			if err != nil {
				return fmt.Errorf("nonce: %w", err)
			}
		}
		msg, err = cp.Ping(ctx, n)
		if err != nil {
			return fmt.Errorf("ping: %w", err)
		}

	case "remote":
		msg, err = cp.Remote()
		if err != nil {
			return fmt.Errorf("remote: %w", err)
		}

	default:
		return fmt.Errorf("invalid action: %v", action)
	}

	out := args["out"]
	switch out {
	case "json":
		j, err := json.MarshalIndent(msg, "", "  ")
		if err != nil {
			return fmt.Errorf("json: %w", err)
		}
		fmt.Printf("%v\n", string(j))

	case "", "spew":
		spew.Dump(msg)

	case "raw":
		err := msg.BtcEncode(bufio.NewWriter(os.Stdout), wire.ProtocolVersion,
			wire.LatestEncoding)
		if err != nil {
			return fmt.Errorf("raw: %w", err)
		}

	default:
		return fmt.Errorf("invalid out: %v", out)
	}

	return nil
}

type bssClient struct {
	wg     *sync.WaitGroup
	bssURL string
}

func (bsc *bssClient) handleBSSWebsocketReadUnauth(ctx context.Context, conn *protocol.Conn) {
	defer bsc.wg.Done()

	log.Tracef("handleBSSWebsocketReadUnauth")
	defer log.Tracef("handleBSSWebsocketReadUnauth exit")
	for {
		// See if we were terminated
		select {
		case <-ctx.Done():
			return
		default:
		}

		cmd, rid, payload, err := bssapi.ReadConn(ctx, conn)
		if err != nil {
			log.Errorf("handleBSSWebsocketReadUnauth: %v", err)
			time.Sleep(3 * time.Second)
			continue
			// return
		}
		log.Infof("cmd: %v rid: %v payload: %T", cmd, rid, payload)
	}
}

func (bsc *bssClient) connect(ctx context.Context) error {
	log.Tracef("connect")
	defer log.Tracef("connect exit")

	conn, err := protocol.NewConn(bsc.bssURL, nil)
	if err != nil {
		return err
	}
	err = conn.Connect(ctx)
	if err != nil {
		return err
	}

	bsc.wg.Add(1)
	go bsc.handleBSSWebsocketReadUnauth(ctx, conn)

	// Required ping
	// _, _, _, err = bssapi.Call(ctx, conn, bssapi.PingRequest{
	//	Timestamp: time.Now().Unix(),
	// })
	// if err != nil {
	//	return fmt.Errorf("ping error: %w", err)
	// }

	simulatePingPong := false
	if simulatePingPong {
		bsc.wg.Add(1)
		go func() {
			defer bsc.wg.Done()
			for {
				// See if we were terminated
				select {
				case <-ctx.Done():
					return
				default:
				}

				time.Sleep(5 * time.Second)
				_, _, _, err = bssapi.Call(ctx, conn, bssapi.PingRequest{
					Timestamp: time.Now().Unix(),
				})
				if err != nil {
					log.Errorf("ping error: %v", err)
					continue
					// return fmt.Errorf("ping error: %w", err)
				}
			}
		}()
	}

	// Wait for exit
	bsc.wg.Wait()

	return nil
}

func (bsc *bssClient) connectBSS(ctx context.Context) {
	log.Tracef("bssClient")
	defer log.Tracef("bssClient exit")

	log.Infof("Connecting to: %v", bsc.bssURL)
	for {
		if err := bsc.connect(ctx); err != nil {
			// Do nothing
			log.Errorf("connect: %v", err) // remove this, too loud
		}
		// See if we were terminated
		select {
		case <-ctx.Done():
			return
		default:
		}

		// hold off reconnect for a couple of seconds
		time.Sleep(5 * time.Second)
		log.Debugf("Reconnecting to: %v", bsc.bssURL)
	}
}

func bssLong(ctx context.Context) error {
	bsc := &bssClient{
		wg:     new(sync.WaitGroup),
		bssURL: bssURL,
	}

	go bsc.connectBSS(ctx)

	<-ctx.Done()
	if !errors.Is(ctx.Err(), context.Canceled) {
		return ctx.Err()
	}

	return nil
}

func client(which string) error {
	log.Debugf("client %v", which)
	defer log.Debugf("client %v exit", which)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	simulateCtrlC := false
	if simulateCtrlC {
		// XXX add signal handler instead of this poop
		go func() {
			time.Sleep(3 * time.Second)
			cancel()
		}()

		defer func() {
			log.Infof("waiting for exit")
			time.Sleep(3 * time.Second)
		}()
	}

	switch which {
	case "bss":
		return bssLong(ctx)
	}
	return fmt.Errorf("invalid client: %v", which)
}

var (
	reSkip         = regexp.MustCompile(`(?i)(Response|Notification)$`)
	allCommands    = make(map[string]reflect.Type)
	sortedCommands []string
)

func init() {
	version.Component = "hemictl"
	welcome = "Hemi Network Controller " + version.BuildInfo()

	// merge all command maps
	for k, v := range bssapi.APICommands() {
		allCommands[string(k)] = v
	}
	for k, v := range bfgapi.APICommands() {
		allCommands[string(k)] = v
	}
	for k, v := range tbcapi.APICommands() {
		allCommands[string(k)] = v
	}

	sortedCommands = make([]string, 0, len(allCommands))
	for k := range allCommands {
		sortedCommands = append(sortedCommands, k)
	}
	sort.Strings(sortedCommands)
}

func usage() {
	fmt.Fprintf(os.Stderr, "%v\n", welcome)
	fmt.Fprintf(os.Stderr, "\t%v <command> [payload]\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "Usage:\n")
	fmt.Fprintf(os.Stderr, "\tbfgdb database connection\n")
	fmt.Fprintf(os.Stderr, "\tbss-client long connection to bss\n")
	fmt.Fprintf(os.Stderr, "\thelp (this help)\n")
	fmt.Fprintf(os.Stderr, "\thelp-verbose JSON print RPC default request/response\n")
	//nolint:dupword // command help, not sentence.
	fmt.Fprintf(os.Stderr, "\tp2p p2p commands\n")
	fmt.Fprintf(os.Stderr, "\ttbcdb datase open (tbcd must not be running)\n")
	fmt.Fprintf(os.Stderr, "Environment:\n")
	config.Help(os.Stderr, cm)
	fmt.Fprintf(os.Stderr, "Commands:\n")
	for _, v := range sortedCommands {
		if reSkip.MatchString(v) {
			continue
		}
		fmt.Fprintf(os.Stderr, "\t%v [%v]\n", v, allCommands[v])
	}
}

func helpVerbose() {
	fmt.Fprintf(os.Stderr, "%v\n", welcome)
	fmt.Fprintf(os.Stderr, "\t%v <command> [payload]\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "Payload request/response/notification:\n")
	for _, v := range sortedCommands {
		cmdType := allCommands[v]
		clone := reflect.New(cmdType).Interface()
		fmt.Fprintf(os.Stderr, "%v:\n", v)
		_ = printJSON(os.Stderr, "  ", clone)
		fmt.Fprintf(os.Stderr, "\n")
	}
}

func printJSON(where io.Writer, indent string, payload any) error {
	w := &bytes.Buffer{}
	fmt.Fprint(where, indent)
	e := json.NewEncoder(w)
	e.SetIndent(indent, "    ")
	if err := e.Encode(payload); err != nil {
		return fmt.Errorf("can't encode payload %T: %w", payload, err)
	}
	fmt.Fprint(where, w.String())
	return nil
}

func HandleSignals(ctx context.Context, cancel context.CancelFunc, callback func(os.Signal)) {
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM)
	defer func() {
		signal.Stop(signalChan)
		cancel()
	}()

	select {
	case <-ctx.Done():
	case s := <-signalChan: // First signal, cancel context.
		if callback != nil {
			callback(s) // Do whatever caller wants first.
			cancel()
		}
	}
	<-signalChan // Second signal, hard exit.
	os.Exit(2)
}

func _main() error {
	if len(os.Args) < 2 {
		usage()
		return errors.New("not enough parameters")
	}

	if err := config.Parse(cm); err != nil {
		return err
	}

	if err := loggo.ConfigureLoggers(logLevel); err != nil {
		return err
	}
	log.Debugf("%v", welcome)

	pc := config.PrintableConfig(cm)
	for k := range pc {
		log.Debugf("%v", pc[k])
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go HandleSignals(ctx, cancel, func(s os.Signal) {
		log.Infof("hemi received signal: %s", s)
	})

	cmd := flag.Arg(0) // command provided by user

	// Deal with non-generic commands
	// XXX fix all this shit
	switch cmd {
	case "bfgdb":
		return bfgdb()
	case "bss-client":
		return client("bss")
	case "help":
		usage()
		return nil
	case "help-verbose":
		helpVerbose()
		return nil
	case "p2p":
		return p2p()
	case "tbcdb":
		return tbcdb(ctx)
	}

	// Deal with generic commands
	cmdType, ok := allCommands[cmd]
	if !ok {
		return fmt.Errorf("unknown command: %v", cmd)
	}
	// Figure out where and what we are calling based on command.
	var (
		u           string
		callHandler func(context.Context, *protocol.Conn)
		call        func(context.Context, *protocol.Conn, any) (protocol.Command, string, any, error)
	)
	switch {
	case strings.HasPrefix(cmd, "bssapi"):
		u = bssapi.DefaultURL
		callHandler = handleBSSWebsocketReadUnauth
		call = bssapi.Call // XXX yuck
	case strings.HasPrefix(cmd, "bfgapi"):
		u = bfgapi.DefaultPrivateURL
		callHandler = handleBFGWebsocketReadUnauth
		call = bfgapi.Call // XXX yuck
	case strings.HasPrefix(cmd, "tbcapi"):
		u = tbcapi.DefaultURL
		callHandler = handleTBCWebsocketRead
		call = tbcapi.Call // XXX yuck?
	default:
		return fmt.Errorf("can't derive URL from command: %v", cmd)
	}
	conn, err := protocol.NewConn(u, &protocol.ConnOptions{
		ReadLimit: tbcReadLimit,
	})
	if err != nil {
		return err
	}
	defer conn.Close()

	tctx, tcancel := context.WithTimeout(ctx, callTimeout)
	defer tcancel()
	go callHandler(tctx, conn) // Make sure we can use Call

	clone := reflect.New(cmdType).Interface()
	log.Debugf("%v", spew.Sdump(clone))
	if flag.Arg(1) != "" {
		err := json.Unmarshal([]byte(flag.Arg(1)), &clone)
		if err != nil {
			return fmt.Errorf("invalid payload: %w", err)
		}
	}
	_, _, payload, err := call(tctx, conn, clone)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	log.Debugf("%v", spew.Sdump(payload))

	return printJSON(os.Stdout, "", payload)
}

func main() {
	flag.Parse()
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	if err := _main(); err != nil {
		fmt.Fprintf(os.Stderr, "\n%v: %v\n", daemonName, err)
		os.Exit(1)
	}
}
