package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jawher/mow.cli"
	log "github.com/sirupsen/logrus"
	"github.com/xlab/catcher"
	"github.com/xlab/closer"

	"github.com/AtlantPlatform/atlant-go/api"
	"github.com/AtlantPlatform/atlant-go/authcenter"
	"github.com/AtlantPlatform/atlant-go/contracts"
	"github.com/AtlantPlatform/atlant-go/fs"
	"github.com/AtlantPlatform/atlant-go/rs"
	"github.com/AtlantPlatform/atlant-go/state"
)

var app = cli.App("atlant-go", "ATLANT Node")
var appVersion = "1.0.0-rc1"

var (
	ipfsConfigFile    = "config"
	ipfsKeyFile       = "swarm.key"
	ipfsKeyDataPrefix = "/key/swarm/psk/1.0.0/\n/base16/\n"
)
var (
	testingCommands []testingCmd
)

type testingCmd struct {
	Name string
	Desc string
	Init cli.CmdInitializer
}

func main() {
	app.Command("init", "Initialize node and its IPFS repo.", nodeInitCmd)
	app.Command("version", "Show version info.", versionCmd)
	for _, cmd := range testingCommands {
		if len(cmd.Name) == 0 {
			panic("found an unnamed testing command")
		} else if !strings.HasPrefix(cmd.Name, "test-") {
			panic("found a testing command that has incorrect name: " + cmd.Name)
		}
		app.Command(cmd.Name, cmd.Desc, cmd.Init)
	}
	logLevel = app.String(cli.StringOpt{
		Name:   "l log-level",
		Desc:   "Logging verbosity (0 = minimum, 1...4, 5 = debug).",
		EnvVar: "AN_LOG_LEVEL",
		Value:  defaultLogLevel,
	})

	app.Before = func() {
		log.SetLevel(log.Level(toNatural(*logLevel, 4)))
		if log.GetLevel() <= log.InfoLevel {
			gin.SetMode(gin.DebugMode)
		} else {
			gin.SetMode(gin.ReleaseMode)
		}
		log.Debugf("set app logging to %v", log.GetLevel())
		procs := runtime.GOMAXPROCS(toNatural(*goMaxProcs, 128))
		log.Debugf("set GOMAXPROCS to %d", procs)

		if len(*logDir) > 0 {
			if err := os.MkdirAll(*logDir, 0755); err != nil {
				log.Warningln("failed to create logs dir:", err)
			} else if rotatingLogger, err := newRotatingLogger(*logDir); err != nil {
				log.Warningln("failed init rotating logs:", err)
			} else {
				logger = rotatingLogger
				log.AddHook(rotatingLogger)
				log.SetOutput(os.Stderr)
				closer.Bind(func() {
					rotatingLogger.Close()
				})
			}
		}
	}
	app.Action = func() {
		var hasTestnetMark bool
		if info, err := os.Stat(filepath.Join(*fsDir, "testnet")); err == nil && !info.IsDir() {
			hasTestnetMark = true
		}
		if hasTestnetMark {
			*envTestnet = true
		}
		if *envTestnet {
			if !hasTestnetMark {
				log.Fatalln("refusing to start in a testnet mode: not initialized for testnet.")
			}
			if *envTestnetKey != testKey {
				log.Warningln("overriding testnet key works only upon initialization, no effect now.")
			}
			domains := append(*envTestnetDomains, authcenter.DefaultTestDomains...)
			authcenter.InitWithDomains(domains)
			log.Println("ATLANT TestNet welcomes you!")
		} else {
			if len(*envTestnetDomains) > 0 {
				log.Warningln("overriding DNS auth domains works only within testnet, no effect now.")
			}
			if *envTestnetKey != testKey {
				log.Warningln("overriding testnet key works only within testnet, no effect now.")
			}
			log.Println("ATLANT MainNet welcomes you!")
		}
		runWithPlanetaryContext(func(ctx PlanetaryContext) {
			defer catcher.Catch(catcher.RecvWrite(logger, true))
			log.Println("Node ID:", ctx.NodeID())
			log.Println("Session ID:", ctx.SessionID())
			if len(*clusterName) == 0 {
				*clusterName = ctx.SessionID()
			}
			store, err := rs.NewPlanetaryRecordStore(ctx.NodeID(), ctx.FileStore(), ctx.StateStore())
			if err != nil {
				log.Fatalln(err)
			}

			closer.Bind(func() {
				log.Debugln("closing record store")
				if err := store.Close(); err != nil {
					log.Warningln(err)
				}
				log.Debugln("waiting for queues")
				wg := new(sync.WaitGroup)
				wg.Add(2)
				go func() {
					defer wg.Done()
					store.WaitInbound(2 * time.Minute)
				}()
				go func() {
					defer wg.Done()
					store.WaitOutbound(2 * time.Minute)
				}()
				wg.Wait()
			})

			*ethAddress = strings.ToLower(*ethAddress)
			mgr := contracts.NewManager(ctx.SessionID(), store, *envTestnet)
			apiCtx := api.NewContext(ctx, store, mgr, *ethAddress, *logDir)
			privateServer := api.NewPrivateServer()
			privateServer.RouteAPI(apiCtx)
			privAddr, err := privateServer.Listen("127.0.0.1:0")
			if err != nil {
				log.Fatalln(err)
			}
			host, port, _ := net.SplitHostPort(privAddr)
			privMultiAddr := fmt.Sprintf("/ip4/%s/tcp/%s", host, port)
			if err := ctx.FileStore().Listener().Listen(privMultiAddr); err != nil {
				log.Fatalln(err)
			}

			time.Sleep(duration(*fsWarmupDur, 5*time.Second))
			if err := store.Sync(); err != nil {
				log.Errorln(err)
				closer.Fatalln(err)
			}
			if len(*ethAddress) > 0 && len(*ethAddress) < 64 {
				go store.SendBeats(ctx, 10*time.Minute, 60*time.Minute, *ethAddress)
			}
			if authcenter.Default.HasPermissions(ctx.NodeID(), authcenter.RecordWritePermission) {
				log.Infoln("this node has interplanetary write permissions")
				go store.CommitBeatReports(ctx, 60*time.Minute)
			}

			publicServer := api.NewPublicServer()
			publicServer.RouteAPI(apiCtx)
			go func() {
				if err := publicServer.ListenAndServe(*webListenAddr); err != nil {
					log.Fatalln(err)
				}
			}()

			closer.Hold()
		})
	}
	if err := app.Run(os.Args); err != nil {
		log.Fatalln(err)
	}
}

func runWithPlanetaryContext(fn func(ctx PlanetaryContext)) {
	defer closer.Close()
	closer.Bind(func() {
		log.Println("atlant-go node is shut down. Bye!")
	})
	log.Println("atlant-go node is starting")

	fsHost, fsPort, err := net.SplitHostPort(*fsListenAddr)
	if err != nil {
		log.Warningf("failed to parse specified listen addr %s: %v", *fsListenAddr, err)
	}
	log.Debugf("using %s as state dir", *stateDir)
	if err := os.MkdirAll(*stateDir, 0700); err != nil {
		log.Fatalln("failed to create state dir:", err)
	}
	log.Debugf("using %s as fs dir", *fsDir)
	if err := os.MkdirAll(*fsDir, 0700); err != nil {
		log.Fatalln("failed to create fs dir:", err)
	}
	if !fileNotEmpty(filepath.Join(*fsDir, ipfsKeyFile)) ||
		!fileNotEmpty(filepath.Join(*fsDir, ipfsConfigFile)) {
		log.Fatalln("fs dir is not initialized, please run atlant-go init")
	}
	log.WithFields(log.Fields{
		"Dir":     *fsDir,
		"Host":    fsHost,
		"Port":    fsPort,
		"Profile": *fsNetworkProfile,
	}).Println("IPFS node warmup in progress")
	if *envTestnet {
		if *envTestnetKey == testKey {
			*fsBootstrapPeers = append(*fsBootstrapPeers, testBootstrapPeers...)
		}
	} else {
		*fsBootstrapPeers = append(*fsBootstrapPeers, mainBootstrapPeers...)
	}
	fileStore, err := fs.NewPlanetaryFileStore(*fsDir,
		fs.UseBootstrapPeersOpt(*fsBootstrapPeers),
		fs.UseRelayOpt(toBool(*fsRelayEnabled)),
		fs.ListenHostOpt(fsHost),
		fs.ListenPortOpt(fsPort),
		fs.UseNetworkProfileOpt(fs.NetworkProfile(*fsNetworkProfile)),
	)
	if err != nil {
		closer.Fatalln("NewPlanetaryFileStore failed:", err)
	}
	closer.Bind(func() {
		if err := fileStore.Close(); err != nil {
			log.Warningf("failed to close IPFS store: %v", err)
		}
	})
	stateStore, err := state.NewIndexedStoreBadger(*stateDir)
	if err != nil {
		closer.Fatalln("NewIndexedStoreBadger failed:", err)
	}
	closer.Bind(func() {
		if err := stateStore.Close(); err != nil {
			log.Warningf("failed to close the state store: %v", err)
		}
	})
	if err := func() (err error) {
		defer catcher.Catch(catcher.RecvError(&err, true))
		env := "main"
		if *envTestnet {
			env = "test"
		}
		ctx := NewPlanetaryContext(context.Background(), env, appVersion, fileStore, stateStore)
		fn(ctx)
		return
	}(); err != nil {
		closer.Fatalln(err)
	}
}

func versionCmd(c *cli.Cmd) {
	c.Action = func() {
		fmt.Fprintf(os.Stdout, "atlant-go version %s\n", appVersion)
		fmt.Fprintln(os.Stderr, `atlant-go Copyright (C) 2018 ATLANT
    This program comes with ABSOLUTELY NO WARRANTY; for details see LICENSE.
    This is free software, and you are welcome to redistribute it
    under certain conditions; governored by GNU GPLv3 license.`)
	}
}

func nodeInitCmd(c *cli.Cmd) {
	c.Action = func() {
		log.Println("atlant-go init")

		log.Debugf("using %s as state dir", *stateDir)
		if err := os.MkdirAll(*stateDir, 0700); err != nil {
			log.Fatalln("failed to create state dir:", err)
		}
		log.Debugf("using %s as fs dir", *fsDir)
		if err := os.MkdirAll(*fsDir, 0700); err != nil {
			log.Fatalln("failed to create fs dir:", err)
		}
		var skipInit bool
		configPath := filepath.Join(*fsDir, ipfsConfigFile)
		if fileNotEmpty(configPath) {
			skipInit = true
			log.WithFields(log.Fields{
				"Dir":  *fsDir,
				"File": configPath,
			}).Errorln("refusing to init IPFS node: config exists")
		}
		if skipInit {
			return
		}
		keyPath := filepath.Join(*fsDir, ipfsKeyFile)
		if fileNotEmpty(keyPath) {
			log.WithFields(log.Fields{
				"Dir":  *fsDir,
				"File": keyPath,
			}).Warnln("overwriting IPFS swarm key file")
		}
		ipfsKeyData := []byte(ipfsKeyDataPrefix + mainKey)
		if *envTestnet {
			log.Println("initilizing within ATLANT Node TestNet")
			ipfsKeyData = []byte(ipfsKeyDataPrefix + *envTestnetKey)
			err := ioutil.WriteFile(filepath.Join(*fsDir, "testnet"), nil, 0600)
			if err != nil {
				log.Fatalf("failed to create a testnet mark file: %v", err)
			}
		} else {
			log.Println("initilizing within ATLANT Node MainNet")
		}
		if err := ioutil.WriteFile(keyPath, ipfsKeyData, 0600); err != nil {
			log.WithFields(log.Fields{
				"File": keyPath,
			}).Fatalln("failed to write private key for IPFS swarm:", err)
		} else {
			log.WithFields(log.Fields{
				"File": keyPath,
			}).Println("generated new private key for IPFS swarm")
		}
		log.WithFields(log.Fields{
			"Dir":      *fsDir,
			"SwarmKey": keyPath,
		}).Println("initialization of new IPFS node in progress")
		fileStore, err := fs.InitPlanetaryFileStore(*fsDir)
		if err != nil {
			log.Fatalln("InitPlanetaryFileStore failed:", err)
		}
		if err := fileStore.Close(); err != nil {
			log.Warnf("failed to close store: %v", err)
		}
		fmt.Println(fileStore.NodeID())
	}
}

func fileNotEmpty(path string) bool {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false
	} else if err != nil {
		log.Errorf("fileNotEmpty: check error: %v", err)
		// unknown error, maybe exists
		return true
	}
	if info.IsDir() {
		return true
	}
	if info.Size() > 0 {
		return true
	}
	return false
}
