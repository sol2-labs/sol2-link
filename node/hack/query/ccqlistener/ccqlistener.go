// This tool can be used to verify that a guardian is properly receiving CCQ queries and publishing responses.

// This tool can be used to passively listen for query responses from any guardian:
//    go run ccqlistener.go --listenOnly
//
// Or it can be used to passively listen for query responses from a particular guardian:
//    go run ccqlistener.go --listenOnly --targetPeerId <yourGuardianP2pPeerId>
//
// This should work both in mainnet and testnet because there are routine monitoring queries running every few minutes.
// Note that you may need to wait 15 minutes or more to see something. Look for message saying "query response received".

// This tool can also be used to generate a simple query request, send it and wait for the response. Note that this takes
// considerable more set up, as the P2P ID and signing key of this tool must be defined here and configured on the guardian.
//
// To use this tool to generate a query, you need two local files:
//    ./ccqlistener.nodeKey - This file contains the P2P peer ID to be used. If the file does not exist, it will be created.
//    ./ccqlistener.signerKey - This file contains the key used to sign the request. It must exist.
//
// If the nodeKey file does not exist, it will be generated. The log output will print the peerID in the "Test started" line.
// That peerID must be included in the `ccqAllowedPeers` parameter on the guardian.
//
// The signerKey file can be generated by doing: guardiand keygen --block-type "CCQ SERVER SIGNING KEY" /path/to/key/file
// The generated key (which is listed as the `PublicKey` in the file) must be included in the `ccqAllowedRequesters` parameter on the guardian.
//
// To run this tool, do `go run ccqlistener.go`
//
// - Look for the line saying "Signing key loaded" and confirm the public key matches what is configured on the guardian.
// - Look for the "Test started" and confirm that the peer ID matches what is configured on the guardian.
// - You should see a line saying "Waiting for peers". If you do not, then the test is unable to bootstrap with any guardians.
// - After a few minutes, you should see a message saying "Got peers". If you do not, then test is unable to communicate with any guardians.
// - After this, the test runs, and you should eventually see "Success! Test passed"
//
// To run the tool as a docker image, you can do something like this:
// - wormhole$ docker build --target build -f node/hack/query/ccqlistener/Dockerfile -t ccqlistener .
// - wormhole$ docker run -v /ccqlistener/cfg:/app/cfg ccqlistener /ccqlistener --configDir /app/cfg
// Where /ccqlistener is a directory containing these files:
// - ccqlistener.nodeKey
// - ccqlistener.signerKey

package main

import (
	"context"
	"crypto/ecdsa"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/certusone/wormhole/node/pkg/common"
	"github.com/certusone/wormhole/node/pkg/p2p"
	gossipv1 "github.com/certusone/wormhole/node/pkg/proto/gossip/v1"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/crypto"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

var (
	p2pNetworkID = flag.String("network", "/wormhole/mainnet/2", "P2P network identifier")
	p2pPort      = flag.Int("port", 8998, "P2P UDP listener port")
	p2pBootstrap = flag.String("bootstrap",
		"/dns4/wormhole-mainnet-v2-bootstrap.certus.one/udp/8996/quic/p2p/12D3KooWQp644DK27fd3d4Km3jr7gHiuJJ5ZGmy8hH4py7fP4FP7,/dns4/wormhole-v2-mainnet-bootstrap.xlabs.xyz/udp/8996/quic/p2p/12D3KooWNQ9tVrcb64tw6bNs2CaNrUGPM7yRrKvBBheQ5yCyPHKC,/dns4/wormhole.mcf.rocks/udp/8996/quic/p2p/12D3KooWDZVv7BhZ8yFLkarNdaSWaB43D6UbQwExJ8nnGAEmfHcU,/dns4/wormhole-v2-mainnet-bootstrap.staking.fund/udp/8996/quic/p2p/12D3KooWG8obDX9DNi1KUwZNu9xkGwfKqTp2GFwuuHpWZ3nQruS1",
		"P2P bootstrap peers (comma-separated)")
	nodeKeyPath   = flag.String("nodeKey", "ccqlistener.nodeKey", "Path to node key (will be generated if it doesn't exist)")
	signerKeyPath = flag.String("signerKey", "ccqlistener.signerKey", "Path to key used to sign unsigned queries")
	configDir     = flag.String("configDir", ".", "Directory where nodeKey and signerKey are loaded from (default is .)")
	listenOnly    = flag.Bool("listenOnly", false, "Only listen for responses, don't publish anything (default is false)")
	targetPeerId  = flag.String("targetPeerId", "", "Only process responses from this peer ID (default is everything)")
)

func main() {

	//
	// BEGIN SETUP
	//

	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger, _ := zap.NewDevelopment()

	nodeKey := *configDir + "/" + *nodeKeyPath

	var err error
	var sk *ecdsa.PrivateKey
	if !*listenOnly {
		signerKey := *configDir + "/" + *signerKeyPath
		logger.Info("Loading signing key", zap.String("signingKeyPath", signerKey))
		sk, err = common.LoadArmoredKey(signerKey, CCQ_SERVER_SIGNING_KEY, true)
		if err != nil {
			logger.Fatal("failed to load guardian key", zap.Error(err))
		}
		logger.Info("Signing key loaded", zap.String("publicKey", ethCrypto.PubkeyToAddress(sk.PublicKey).Hex()))
	}

	// Load p2p private key
	var priv crypto.PrivKey
	priv, err = common.GetOrCreateNodeKey(logger, nodeKey)
	if err != nil {
		logger.Fatal("Failed to load node key", zap.Error(err))
	}

	// Manual p2p setup
	components := p2p.DefaultComponents()
	components.Port = uint(*p2pPort)
	bootstrapPeers := *p2pBootstrap
	networkID := *p2pNetworkID + "/ccq"

	h, err := p2p.NewHost(logger, ctx, networkID, bootstrapPeers, components, priv)
	if err != nil {
		panic(err)
	}

	topic_req := fmt.Sprintf("%s/%s", networkID, "ccq_req")
	topic_resp := fmt.Sprintf("%s/%s", networkID, "ccq_resp")

	logger.Info("Subscribing pubsub topic", zap.String("topic_req", topic_req), zap.String("topic_resp", topic_resp))
	ps, err := pubsub.NewGossipSub(ctx, h)
	if err != nil {
		panic(err)
	}

	th_req, err := ps.Join(topic_req)
	if err != nil {
		logger.Panic("failed to join request topic", zap.String("topic_req", topic_req), zap.Error(err))
	}

	th_resp, err := ps.Join(topic_resp)
	if err != nil {
		logger.Panic("failed to join response topic", zap.String("topic_resp", topic_resp), zap.Error(err))
	}

	sub, err := th_resp.Subscribe()
	if err != nil {
		logger.Panic("failed to subscribe to response topic", zap.Error(err))
	}

	logger.Info("Test started", zap.String("peer_id", h.ID().String()),
		zap.String("addrs", fmt.Sprintf("%v", h.Addrs())))

	// Wait for peers
	logger.Info("Waiting for peers")
	for len(th_req.ListPeers()) < 1 {
		time.Sleep(time.Millisecond * 100)
	}
	logger.Info("Got peers", zap.Int("numPeers", len(th_req.ListPeers())))

	// Handle SIGTERM
	sigterm := make(chan os.Signal, 1)
	signal.Notify(sigterm, syscall.SIGTERM)
	signal.Notify(sigterm, syscall.SIGINT)
	go func() {
		<-sigterm
		logger.Info("Received sigterm. exiting.")
		cancel()
	}()

	//
	// END SETUP
	//

	if *listenOnly {
		listenForMessages(ctx, logger, sub)
	}

	// Cleanly shutdown
	// Without this the same host won't properly discover peers until some timeout
	sub.Cancel()
	if err := th_req.Close(); err != nil {
		logger.Fatal("Error closing the request topic", zap.Error(err))
	}
	if err := th_resp.Close(); err != nil {
		logger.Fatal("Error closing the response topic", zap.Error(err))
	}
	if err := h.Close(); err != nil {
		logger.Fatal("Error closing the host", zap.Error(err))
	}

	//
	// END SHUTDOWN
	//

	logger.Info("Success! Test passed!")
}

const (
	CCQ_SERVER_SIGNING_KEY = "CCQ SERVER SIGNING KEY"
)

func listenForMessages(ctx context.Context, logger *zap.Logger, sub *pubsub.Subscription) {
	if *targetPeerId == "" {
		logger.Info("Will not publish, only listening for messages from all peers...")
	} else {
		logger.Info("Will not publish, only listening for messages from a single peer...", zap.String("peerID", *targetPeerId))
	}
	for {
		envelope, err := sub.Next(ctx)
		if err != nil {
			logger.Panic("failed to receive pubsub message", zap.Error(err))
		}
		var msg gossipv1.GossipMessage
		err = proto.Unmarshal(envelope.Data, &msg)
		if err != nil {
			logger.Info("received invalid message",
				zap.Binary("data", envelope.Data),
				zap.String("from", envelope.GetFrom().String()))
			continue
		}
		switch m := msg.Message.(type) {
		case *gossipv1.GossipMessage_SignedQueryResponse:
			if *targetPeerId != "" && envelope.GetFrom().String() != *targetPeerId {
				continue
			}
			logger.Info("query response received",
				zap.String("from", envelope.GetFrom().String()),
				zap.Any("response", m.SignedQueryResponse),
				zap.String("responseBytes", hexutil.Encode(m.SignedQueryResponse.QueryResponse)),
				zap.String("sigBytes", hexutil.Encode(m.SignedQueryResponse.Signature)))
		default:
			continue
		}
	}
}
