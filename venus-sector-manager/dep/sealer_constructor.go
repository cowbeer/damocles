package dep

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"

	"github.com/BurntSushi/toml"
	"go.uber.org/fx"

	"github.com/filecoin-project/go-jsonrpc"
	vapi "github.com/filecoin-project/venus/venus-shared/api"

	"github.com/ipfs-force-community/venus-cluster/venus-sector-manager/core"
	"github.com/ipfs-force-community/venus-cluster/venus-sector-manager/modules"
	"github.com/ipfs-force-community/venus-cluster/venus-sector-manager/modules/impl/commitmgr"
	"github.com/ipfs-force-community/venus-cluster/venus-sector-manager/modules/impl/dealmgr"
	"github.com/ipfs-force-community/venus-cluster/venus-sector-manager/modules/impl/mock"
	"github.com/ipfs-force-community/venus-cluster/venus-sector-manager/modules/impl/sectors"
	"github.com/ipfs-force-community/venus-cluster/venus-sector-manager/modules/impl/worker"
	"github.com/ipfs-force-community/venus-cluster/venus-sector-manager/pkg/chain"
	"github.com/ipfs-force-community/venus-cluster/venus-sector-manager/pkg/confmgr"
	"github.com/ipfs-force-community/venus-cluster/venus-sector-manager/pkg/homedir"
	"github.com/ipfs-force-community/venus-cluster/venus-sector-manager/pkg/kvstore"
	"github.com/ipfs-force-community/venus-cluster/venus-sector-manager/pkg/market"
	"github.com/ipfs-force-community/venus-cluster/venus-sector-manager/pkg/messager"
	"github.com/ipfs-force-community/venus-cluster/venus-sector-manager/pkg/objstore"
	"github.com/ipfs-force-community/venus-cluster/venus-sector-manager/pkg/objstore/filestore"
	"github.com/ipfs-force-community/venus-cluster/venus-sector-manager/pkg/piecestore"
)

type (
	OnlineMetaStore             kvstore.KVStore
	OfflineMetaStore            kvstore.KVStore
	PersistedObjectStoreManager objstore.Manager
	SectorIndexMetaStore        kvstore.KVStore
	ListenAddress               string
	ProxyAddress                string
	WorkerMetaStore             kvstore.KVStore
	ConfDirPath                 string
)

func BuildLocalSectorManager(scfg *modules.SafeConfig, mapi core.MinerInfoAPI, numAlloc core.SectorNumberAllocator) (core.SectorManager, error) {
	return sectors.NewManager(scfg, mapi, numAlloc)
}

func BuildConfDirPath(home *homedir.Home) ConfDirPath {
	return ConfDirPath(home.Dir())
}

func BuildLocalConfigManager(gctx GlobalContext, lc fx.Lifecycle, confDir ConfDirPath) (confmgr.ConfigManager, error) {
	cfgmgr, err := confmgr.NewLocal(string(confDir))
	if err != nil {
		return nil, err
	}

	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			return cfgmgr.Run(gctx)
		},
		OnStop: func(ctx context.Context) error {
			return cfgmgr.Close(ctx)
		},
	})

	return cfgmgr, nil
}

func ProvideConfig(gctx GlobalContext, lc fx.Lifecycle, cfgmgr confmgr.ConfigManager, locker confmgr.WLocker) (*modules.Config, error) {
	cfg := modules.DefaultConfig(false)
	if err := cfgmgr.Load(gctx, modules.ConfigKey, &cfg); err != nil {
		return nil, err
	}

	buf := bytes.Buffer{}
	encode := toml.NewEncoder(&buf)
	encode.Indent = ""
	err := encode.Encode(cfg)
	if err != nil {
		return nil, err
	}

	log.Infof("Sector-manager initial cfg: %s\n", buf.String())

	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			return cfgmgr.Watch(gctx, modules.ConfigKey, &cfg, locker, func() interface{} {
				c := modules.DefaultConfig(false)
				return &c
			})
		},
	})

	return &cfg, nil
}

func ProvideSafeConfig(cfg *modules.Config, locker confmgr.RLocker) (*modules.SafeConfig, error) {
	return &modules.SafeConfig{
		Config: cfg,
		Locker: locker,
	}, nil
}

func BuildOnlineMetaStore(gctx GlobalContext, lc fx.Lifecycle, home *homedir.Home) (OnlineMetaStore, error) {
	dir := home.Sub("meta")
	store, err := kvstore.OpenBadger(kvstore.DefaultBadgerOption(dir))
	if err != nil {
		return nil, err
	}

	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			return store.Run(gctx)
		},

		OnStop: func(ctx context.Context) error {
			return store.Close(ctx)
		},
	})

	return store, nil
}

func BuildOfflineMetaStore(gctx GlobalContext, lc fx.Lifecycle, home *homedir.Home) (OfflineMetaStore, error) {
	dir := home.Sub("offline_meta")
	store, err := kvstore.OpenBadger(kvstore.DefaultBadgerOption(dir))
	if err != nil {
		return nil, err
	}

	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			return store.Run(gctx)
		},

		OnStop: func(ctx context.Context) error {
			return store.Close(ctx)
		},
	})

	return store, nil
}

func BuildSectorNumberAllocator(meta OnlineMetaStore) (core.SectorNumberAllocator, error) {
	store, err := kvstore.NewWrappedKVStore([]byte("sector-number"), meta)
	if err != nil {
		return nil, err
	}

	return sectors.NewNumerAllocator(store)
}

func BuildLocalSectorStateManager(online OnlineMetaStore, offline OfflineMetaStore) (core.SectorStateManager, error) {
	onlineStore, err := kvstore.NewWrappedKVStore([]byte("sector-states"), online)
	if err != nil {
		return nil, err
	}

	offlineStore, err := kvstore.NewWrappedKVStore([]byte("sector-states-offline"), offline)
	if err != nil {
		return nil, err
	}

	return sectors.NewStateManager(onlineStore, offlineStore)
}

func BuildMessagerClient(gctx GlobalContext, lc fx.Lifecycle, scfg *modules.Config, locker confmgr.RLocker) (messager.API, error) {
	locker.Lock()
	api, token := scfg.Common.API.Messager, scfg.Common.API.Token
	locker.Unlock()

	mcli, mcloser, err := messager.New(gctx, api, token)
	if err != nil {
		return nil, err
	}

	lc.Append(fx.Hook{
		OnStop: func(ctx context.Context) error {
			mcloser()
			return nil
		},
	})

	return mcli, nil
}

// used for cli commands
func MaybeSealerCliClient(gctx GlobalContext, lc fx.Lifecycle, listen ListenAddress) core.SealerCliClient {
	cli, err := buildSealerCliClient(gctx, lc, string(listen), false)
	if err != nil {
		cli = core.UnavailableSealerCliClient
	}

	return cli
}

// used for proxy
func BuildSealerProxyClient(gctx GlobalContext, lc fx.Lifecycle, proxy ProxyAddress) (core.SealerCliClient, error) {
	return buildSealerCliClient(gctx, lc, string(proxy), true)
}

func buildSealerCliClient(gctx GlobalContext, lc fx.Lifecycle, serverAddr string, useHTTP bool) (core.SealerCliClient, error) {
	var scli core.SealerCliClient

	addr, err := net.ResolveTCPAddr("tcp", serverAddr)
	if err != nil {
		return scli, err
	}

	ip := addr.IP
	if ip == nil || ip.Equal(net.IPv4zero) {
		ip = net.IPv4(127, 0, 0, 1)
	}

	maddr := fmt.Sprintf("/ip4/%s/tcp/%d", ip, addr.Port)
	if useHTTP {
		maddr += "/http"
	}

	ainfo := vapi.NewAPIInfo(maddr, "")
	apiAddr, err := ainfo.DialArgs(vapi.VerString(core.MajorVersion))
	if err != nil {
		return scli, err
	}

	closer, err := jsonrpc.NewClient(gctx, apiAddr, "Venus", &scli, ainfo.AuthHeader())
	if err != nil {
		return scli, err
	}

	lc.Append(fx.Hook{
		OnStop: func(ctx context.Context) error {
			closer()
			return nil
		},
	})

	return scli, nil
}

func BuildChainClient(gctx GlobalContext, lc fx.Lifecycle, scfg *modules.Config, locker confmgr.RLocker) (chain.API, error) {
	locker.Lock()
	api, token := scfg.Common.API.Chain, scfg.Common.API.Token
	locker.Unlock()

	ccli, ccloser, err := chain.New(gctx, api, token)
	if err != nil {
		return nil, err
	}

	lc.Append(fx.Hook{
		OnStop: func(ctx context.Context) error {
			ccloser()
			return nil
		},
	})

	return ccli, nil
}

func BuildMinerInfoAPI(gctx GlobalContext, lc fx.Lifecycle, capi chain.API, scfg *modules.Config, locker confmgr.RLocker) (core.MinerInfoAPI, error) {
	mapi := chain.NewMinerInfoAPI(capi)

	locker.Lock()
	miners := scfg.Miners
	locker.Unlock()

	if len(miners) > 0 {
		lc.Append(fx.Hook{
			OnStart: func(ctx context.Context) error {
				var wg sync.WaitGroup
				wg.Add(len(miners))

				for i := range miners {
					go func(mi int) {
						defer wg.Done()
						mid := miners[mi].Actor

						mlog := log.With("miner", mid)
						info, err := mapi.Get(gctx, mid)
						if err == nil {
							mlog.Infof("miner info pre-fetched: %#v", info)
						} else {
							mlog.Warnf("miner info pre-fetch failed: %v", err)
						}
					}(i)
				}

				wg.Wait()

				return nil
			},
		})
	}

	return mapi, nil
}

func BuildCommitmentManager(
	gctx GlobalContext,
	lc fx.Lifecycle,
	capi chain.API,
	mapi messager.API,
	rapi core.RandomnessAPI,
	stmgr core.SectorStateManager,
	minfoAPI core.MinerInfoAPI,
	scfg *modules.SafeConfig,
	verif core.Verifier,
	prover core.Prover,
) (core.CommitmentManager, error) {
	mgr, err := commitmgr.NewCommitmentMgr(
		gctx,
		mapi,
		commitmgr.NewSealingAPIImpl(capi, rapi),
		minfoAPI,
		stmgr,
		scfg,
		verif,
		prover,
	)

	if err != nil {
		return nil, err
	}

	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			mgr.Run(gctx)
			return nil
		},

		OnStop: func(ctx context.Context) error {
			mgr.Stop()
			return nil
		},
	})

	return mgr, nil
}

func BuildSectorIndexMetaStore(gctx GlobalContext, lc fx.Lifecycle, home *homedir.Home) (SectorIndexMetaStore, error) {
	dir := home.Sub("sector-index")
	store, err := kvstore.OpenBadger(kvstore.DefaultBadgerOption(dir))
	if err != nil {
		return nil, err
	}

	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			return store.Run(gctx)
		},

		OnStop: func(ctx context.Context) error {
			return store.Close(ctx)
		},
	})

	return store, nil
}

func BuildPersistedFileStoreMgr(scfg *modules.Config, locker confmgr.RLocker) (PersistedObjectStoreManager, error) {
	locker.Lock()
	persistCfg := scfg.Common.PersistStores
	locker.Unlock()

	return filestore.NewManager(persistCfg)
}

func BuildSectorIndexer(storeMgr PersistedObjectStoreManager, kv SectorIndexMetaStore) (core.SectorIndexer, error) {
	upgrade, err := kvstore.NewWrappedKVStore([]byte("sector-upgrade"), kv)
	if err != nil {
		return nil, fmt.Errorf("wrap kvstore for sector-upgrade: %w", err)
	}

	return sectors.NewIndexer(storeMgr, kv, upgrade)
}

func BuildSectorTracker(indexer core.SectorIndexer) (core.SectorTracker, error) {
	return sectors.NewTracker(indexer)
}

type MarketAPIRelatedComponets struct {
	fx.Out

	DealManager core.DealManager
	MarketAPI   market.API
}

func BuildMarketAPI(gctx GlobalContext, lc fx.Lifecycle, scfg *modules.SafeConfig, infoAPI core.MinerInfoAPI) (market.API, error) {
	scfg.Lock()
	api, token := scfg.Common.API.Market, scfg.Common.API.Token
	defer scfg.Unlock()

	if api == "" {
		return nil, nil
	}

	mapi, mcloser, err := market.New(gctx, api, token)
	if err != nil {
		return nil, fmt.Errorf("construct market api: %w", err)
	}

	lc.Append(fx.Hook{
		OnStop: func(ctx context.Context) error {
			mcloser()
			return nil
		},
	})

	return mapi, nil
}

func BuildMarketAPIRelated(gctx GlobalContext, lc fx.Lifecycle, scfg *modules.SafeConfig, infoAPI core.MinerInfoAPI) (MarketAPIRelatedComponets, error) {
	mapi, err := BuildMarketAPI(gctx, lc, scfg, infoAPI)
	if err != nil {
		return MarketAPIRelatedComponets{}, fmt.Errorf("build market api: %w", err)
	}

	if mapi == nil {
		log.Warn("deal manager based on market api is disabled, use mocked")
		return MarketAPIRelatedComponets{
			DealManager: mock.NewDealManager(),
			MarketAPI:   nil,
		}, nil
	}

	scfg.Lock()
	pieceStoreCfg := scfg.Common.PieceStores
	scfg.Unlock()

	locals, err := filestore.OpenMany(pieceStoreCfg)
	if err != nil {
		return MarketAPIRelatedComponets{}, fmt.Errorf("open local piece stores: %w", err)
	}

	proxy := piecestore.NewProxy(locals, mapi)
	http.DefaultServeMux.Handle(HTTPEndpointPiecestore, http.StripPrefix(HTTPEndpointPiecestore, proxy))
	log.Info("piecestore proxy has been registered into default mux")

	return MarketAPIRelatedComponets{
		DealManager: dealmgr.New(mapi, infoAPI, scfg),
		MarketAPI:   mapi,
	}, nil
}

func BuildChainEventBus(
	gctx GlobalContext,
	lc fx.Lifecycle,
	capi chain.API,
	scfg *modules.SafeConfig,
) (*chain.EventBus, error) {
	scfg.Lock()
	interval := scfg.Common.API.ChainEventInterval
	scfg.Unlock()

	bus, err := chain.NewEventBus(gctx, capi, interval.Std())
	if err != nil {
		return nil, fmt.Errorf("construct chain eventbus: %w", err)
	}

	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			go bus.Run()
			return nil
		},

		OnStop: func(ctx context.Context) error {
			bus.Stop()
			return nil
		},
	})

	return bus, nil
}

func BuildSnapUpManager(
	gctx GlobalContext,
	lc fx.Lifecycle,
	home *homedir.Home,
	scfg *modules.SafeConfig,
	tracker core.SectorTracker,
	indexer core.SectorIndexer,
	chainAPI chain.API,
	eventbus *chain.EventBus,
	messagerAPI messager.API,
	minerInfoAPI core.MinerInfoAPI,
	stateMgr core.SectorStateManager,
) (core.SnapUpSectorManager, error) {
	dir := home.Sub("snapup")
	kv, err := kvstore.OpenBadger(kvstore.DefaultBadgerOption(dir))
	if err != nil {
		return nil, err
	}

	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			return kv.Run(gctx)
		},

		OnStop: func(ctx context.Context) error {
			return kv.Close(ctx)
		},
	})

	mgr, err := sectors.NewSnapUpMgr(gctx, tracker, indexer, chainAPI, eventbus, messagerAPI, minerInfoAPI, stateMgr, scfg, kv)
	if err != nil {
		return nil, fmt.Errorf("construct snapup manager: %w", err)
	}

	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			return mgr.Start()
		},

		OnStop: func(ctx context.Context) error {
			mgr.Stop()
			return nil
		},
	})

	return mgr, nil
}

func BuildWorkerMetaStore(gctx GlobalContext, lc fx.Lifecycle, home *homedir.Home) (WorkerMetaStore, error) {
	dir := home.Sub("worker")
	store, err := kvstore.OpenBadger(kvstore.DefaultBadgerOption(dir))
	if err != nil {
		return nil, err
	}

	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			return store.Run(gctx)
		},

		OnStop: func(ctx context.Context) error {
			return store.Close(ctx)
		},
	})

	return store, nil
}

func BuildWorkerManager(meta WorkerMetaStore) (core.WorkerManager, error) {
	return worker.NewManager(meta)
}

func BuildProxiedSectorIndex(client core.SealerCliClient, storeMgr PersistedObjectStoreManager) (core.SectorIndexer, error) {
	log.Debug("build proxied sector indexer")
	return sectors.NewProxiedIndexer(client, storeMgr)
}
