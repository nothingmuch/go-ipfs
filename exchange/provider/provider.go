package provider

import (
	"context"
	"time"

	flags "github.com/ipfs/go-ipfs/flags"

	cid "gx/ipfs/QmNp85zy9RLrQ5oQD4hPyS39ezrrXpcaa7R4Y9kxdWQLLQ/go-cid"
	routing "gx/ipfs/QmPR2JzfKd9poHx9XBhzoFeBBC31ZM3W5iUPKJZWyaoZZm/go-libp2p-routing"
	process "gx/ipfs/QmSF8fPo3jgVBAy8fpdjjYqgG87dkJgUprRBHRd2tmfgpP/goprocess"
	procctx "gx/ipfs/QmSF8fPo3jgVBAy8fpdjjYqgG87dkJgUprRBHRd2tmfgpP/goprocess/context"
	logging "gx/ipfs/QmSpJByNKFX1sCsHBEp3R73FL4NF6FnQTEGyNAXHm2GS52/go-log"
)

var (
	provideKeysBufferSize = 2048
	HasBlockBufferSize    = 256

	provideWorkerMax = 512
	provideTimeout   = time.Second * 15
)

var log = logging.Logger("provider")

// Provider advertises possession of a block to libp2p content routing
type Provider interface {
	Provide(context.Context, *cid.Cid) error
	Stat() (*Stat, error)
}

type provider struct {
	routing routing.ContentRouting

	// newBlocks is a channel for newly added blocks to be provided to the
	// network.  blocks pushed down this channel get buffered and fed to the
	// provideKeys channel later on to avoid too much network activity
	newBlocks chan *cid.Cid
	// provideKeys directly feeds provide workers
	provideKeys chan *cid.Cid
}

func init() {
	if flags.LowMemMode {
		HasBlockBufferSize = 64
		provideKeysBufferSize = 512
		provideWorkerMax = 16
	}
}

func NewProvider(parent context.Context, routing routing.ContentRouting) Provider {
	ctx, cancelFunc := context.WithCancel(parent)

	px := process.WithTeardown(func() error {
		return nil
	})

	p := &provider{
		routing: routing,

		newBlocks:   make(chan *cid.Cid, HasBlockBufferSize),
		provideKeys: make(chan *cid.Cid, provideKeysBufferSize),
	}

	p.startWorkers(px, ctx)
	// bind the context and process.
	// do it over here to avoid closing before all setup is done.
	go func() {
		<-px.Closing() // process closes first
		cancelFunc()
	}()
	procctx.CloseAfterContext(px, ctx) // parent cancelled first

	return p
}

func (p *provider) Provide(ctx context.Context, b *cid.Cid) error {
	return nil
}

func (p *provider) startWorkers(px process.Process, ctx context.Context) {
	// Start up a worker to manage sending out provides messages
	px.Go(func(px process.Process) {
		p.provideCollector(ctx)
	})

	// Spawn up multiple workers to handle incoming blocks
	// consider increasing number if providing blocks bottlenecks
	// file transfers
	px.Go(p.provideWorker)
}

func (p *provider) provideWorker(px process.Process) {

	limit := make(chan struct{}, provideWorkerMax)

	limitedGoProvide := func(k *cid.Cid, wid int) {
		defer func() {
			// replace token when done
			<-limit
		}()
		ev := logging.LoggableMap{"ID": wid}

		ctx := procctx.OnClosingContext(px) // derive ctx from px
		defer log.EventBegin(ctx, "Bitswap.ProvideWorker.Work", ev, k).Done()

		ctx, cancel := context.WithTimeout(ctx, provideTimeout) // timeout ctx
		defer cancel()

		if err := p.routing.Provide(ctx, k, true); err != nil {
			log.Warning(err)
		}
	}

	// worker spawner, reads from bs.provideKeys until it closes, spawning a
	// _ratelimited_ number of workers to handle each key.
	for wid := 2; ; wid++ {
		ev := logging.LoggableMap{"ID": 1}
		log.Event(procctx.OnClosingContext(px), "Bitswap.ProvideWorker.Loop", ev)

		select {
		case <-px.Closing():
			return
		case k, ok := <-p.provideKeys:
			if !ok {
				log.Debug("provideKeys channel closed")
				return
			}
			select {
			case <-px.Closing():
				return
			case limit <- struct{}{}:
				go limitedGoProvide(k, wid)
			}
		}
	}
}

func (p *provider) provideCollector(ctx context.Context) {
	defer close(p.provideKeys)
	var toProvide []*cid.Cid
	var nextKey *cid.Cid
	var keysOut chan *cid.Cid

	for {
		select {
		case blkey, ok := <-p.newBlocks:
			if !ok {
				log.Debug("newBlocks channel closed")
				return
			}

			if keysOut == nil {
				nextKey = blkey
				keysOut = p.provideKeys
			} else {
				toProvide = append(toProvide, blkey)
			}
		case keysOut <- nextKey:
			if len(toProvide) > 0 {
				nextKey = toProvide[0]
				toProvide = toProvide[1:]
			} else {
				keysOut = nil
			}
		case <-ctx.Done():
			return
		}
	}
}
