package query

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/certusone/wormhole/node/pkg/common"
	gossipv1 "github.com/certusone/wormhole/node/pkg/proto/gossip/v1"
	"github.com/wormhole-foundation/wormhole/sdk/vaa"

	ethCommon "github.com/ethereum/go-ethereum/common"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.uber.org/zap"
)

const (
	testSigner = "beFA429d57cD18b7F8A4d91A2da9AB4AF05d0FBe"

	// Magic retry values used to cause special behavior in the watchers.
	fatalError  = math.MaxInt
	ignoreQuery = math.MaxInt - 1

	// Speed things up for testing purposes.
	requestTimeoutForTest = 100 * time.Millisecond
	retryIntervalForTest  = 10 * time.Millisecond
	auditIntervalForTest  = 10 * time.Millisecond
	pollIntervalForTest   = 5 * time.Millisecond
)

var (
	nonce = uint32(0)

	watcherChainsForTest = []vaa.ChainID{vaa.ChainIDPolygon, vaa.ChainIDBSC, vaa.ChainIDArbitrum}
)

// createSignedQueryRequestForTesting creates a query request object and signs it using the specified key.
func createSignedQueryRequestForTesting(
	t *testing.T,
	sk *ecdsa.PrivateKey,
	perChainQueries []*PerChainQueryRequest,
) (*gossipv1.SignedQueryRequest, *QueryRequest) {
	t.Helper()
	nonce += 1
	queryRequest := &QueryRequest{
		Nonce:           nonce,
		PerChainQueries: perChainQueries,
	}

	queryRequestBytes, err := queryRequest.Marshal()
	if err != nil {
		panic(err)
	}

	digest := QueryRequestDigest(common.UnsafeDevNet, queryRequestBytes)
	sig, err := ethCrypto.Sign(digest.Bytes(), sk)
	if err != nil {
		panic(err)
	}

	signedQueryRequest := &gossipv1.SignedQueryRequest{
		QueryRequest: queryRequestBytes,
		Signature:    sig,
	}

	return signedQueryRequest, queryRequest
}

// validateResponseForTest performs validation on the responses generated by these tests. Note that it is not a generalized validate function.
func validateResponseForTest(
	t *testing.T,
	response *QueryResponsePublication,
	signedRequest *gossipv1.SignedQueryRequest,
	queryRequest *QueryRequest,
	expectedResults []PerChainQueryResponse,
) bool {
	require.NotNil(t, response)
	require.True(t, SignedQueryRequestEqual(signedRequest, response.Request))
	require.Equal(t, len(queryRequest.PerChainQueries), len(response.PerChainResponses))
	require.True(t, bytes.Equal(response.Request.Signature, signedRequest.Signature))
	require.Equal(t, len(response.PerChainResponses), len(expectedResults))
	for idx := range response.PerChainResponses {
		require.True(t, response.PerChainResponses[idx].Equal(&expectedResults[idx]))
	}

	return true
}

func TestParseAllowedRequestersSuccess(t *testing.T) {
	ccqAllowedRequestersList, err := parseAllowedRequesters(testSigner)
	require.NoError(t, err)
	require.NotNil(t, ccqAllowedRequestersList)
	require.Equal(t, 1, len(ccqAllowedRequestersList))

	_, exists := ccqAllowedRequestersList[ethCommon.BytesToAddress(ethCommon.Hex2Bytes(testSigner))]
	require.True(t, exists)
	_, exists = ccqAllowedRequestersList[ethCommon.BytesToAddress(ethCommon.Hex2Bytes("beFA429d57cD18b7F8A4d91A2da9AB4AF05d0FBf"))]
	require.False(t, exists)

	ccqAllowedRequestersList, err = parseAllowedRequesters("beFA429d57cD18b7F8A4d91A2da9AB4AF05d0FBe,beFA429d57cD18b7F8A4d91A2da9AB4AF05d0FBf")
	require.NoError(t, err)
	require.NotNil(t, ccqAllowedRequestersList)
	require.Equal(t, 2, len(ccqAllowedRequestersList))

	_, exists = ccqAllowedRequestersList[ethCommon.BytesToAddress(ethCommon.Hex2Bytes(testSigner))]
	require.True(t, exists)
	_, exists = ccqAllowedRequestersList[ethCommon.BytesToAddress(ethCommon.Hex2Bytes("beFA429d57cD18b7F8A4d91A2da9AB4AF05d0FBf"))]
	require.True(t, exists)
}

func TestParseAllowedRequestersFailsIfParameterEmpty(t *testing.T) {
	ccqAllowedRequestersList, err := parseAllowedRequesters("")
	require.Error(t, err)
	require.Nil(t, ccqAllowedRequestersList)

	ccqAllowedRequestersList, err = parseAllowedRequesters(",")
	require.Error(t, err)
	require.Nil(t, ccqAllowedRequestersList)
}

func TestParseAllowedRequestersFailsIfInvalidParameter(t *testing.T) {
	ccqAllowedRequestersList, err := parseAllowedRequesters("Hello")
	require.Error(t, err)
	require.Nil(t, ccqAllowedRequestersList)
}

// mockData is the data structure used to mock up the query handler environment.
type mockData struct {
	sk *ecdsa.PrivateKey

	signedQueryReqReadC  <-chan *gossipv1.SignedQueryRequest
	signedQueryReqWriteC chan<- *gossipv1.SignedQueryRequest

	chainQueryReqC map[vaa.ChainID]chan *PerChainQueryInternal

	queryResponseReadC  <-chan *PerChainQueryResponseInternal
	queryResponseWriteC chan<- *PerChainQueryResponseInternal

	queryResponsePublicationReadC  <-chan *QueryResponsePublication
	queryResponsePublicationWriteC chan<- *QueryResponsePublication

	mutex                    sync.Mutex
	queryResponsePublication *QueryResponsePublication
	expectedResults          []PerChainQueryResponse
	requestsPerChain         map[vaa.ChainID]int
	retriesPerChain          map[vaa.ChainID]int
}

// resetState() is used to reset mock data between queries in the same test.
func (md *mockData) resetState() {
	md.mutex.Lock()
	defer md.mutex.Unlock()
	md.queryResponsePublication = nil
	md.expectedResults = nil
	md.requestsPerChain = make(map[vaa.ChainID]int)
	md.retriesPerChain = make(map[vaa.ChainID]int)
}

// setExpectedResults sets the results to be returned by the watchers.
func (md *mockData) setExpectedResults(expectedResults []PerChainQueryResponse) {
	md.mutex.Lock()
	defer md.mutex.Unlock()
	md.expectedResults = expectedResults
}

// setRetries allows a test to specify how many times a given watcher should retry before returning success.
// If the count is the special value `fatalError`, the watcher will return QueryFatalError.
func (md *mockData) setRetries(chainId vaa.ChainID, count int) {
	md.mutex.Lock()
	defer md.mutex.Unlock()
	md.retriesPerChain[chainId] = count
}

// incrementRequestsPerChainAlreadyLocked is used by the watchers to keep track of how many times they were invoked in a given test.
func (md *mockData) incrementRequestsPerChainAlreadyLocked(chainId vaa.ChainID) {
	if val, exists := md.requestsPerChain[chainId]; exists {
		md.requestsPerChain[chainId] = val + 1
	} else {
		md.requestsPerChain[chainId] = 1
	}
}

// getQueryResponsePublication returns the latest query response publication received by the mock.
func (md *mockData) getQueryResponsePublication() *QueryResponsePublication {
	md.mutex.Lock()
	defer md.mutex.Unlock()
	return md.queryResponsePublication
}

// getRequestsPerChain returns the count of the number of times the given watcher was invoked in a given test.
func (md *mockData) getRequestsPerChain(chainId vaa.ChainID) int {
	md.mutex.Lock()
	defer md.mutex.Unlock()
	if ret, exists := md.requestsPerChain[chainId]; exists {
		return ret
	}
	return 0
}

// shouldIgnoreAlreadyLocked is used by the watchers to see if they should ignore a query (causing a retry).
func (md *mockData) shouldIgnoreAlreadyLocked(chainId vaa.ChainID) bool {
	if val, exists := md.retriesPerChain[chainId]; exists {
		if val == ignoreQuery {
			delete(md.retriesPerChain, chainId)
			return true
		}
	}
	return false
}

// getStatusAlreadyLocked is used by the watchers to determine what query status they should return, based on the `retriesPerChain`.
func (md *mockData) getStatusAlreadyLocked(chainId vaa.ChainID) QueryStatus {
	if val, exists := md.retriesPerChain[chainId]; exists {
		if val == fatalError {
			return QueryFatalError
		}
		val -= 1
		if val > 0 {
			md.retriesPerChain[chainId] = val
		} else {
			delete(md.retriesPerChain, chainId)
		}
		return QueryRetryNeeded
	}
	return QuerySuccess
}

// createQueryHandlerForTest creates the query handler mock environment, including the set of watchers and the response listener.
// Most tests will use this function to set up the mock.
func createQueryHandlerForTest(t *testing.T, ctx context.Context, logger *zap.Logger, chains []vaa.ChainID) *mockData {
	md := createQueryHandlerForTestWithoutPublisher(t, ctx, logger, chains)
	md.startResponseListener(ctx)
	return md
}

// createQueryHandlerForTestWithoutPublisher creates the query handler mock environment, including the set of watchers but not the response listener.
// This function can be invoked directly to test retries of response publication (by delaying the start of the response listener).
func createQueryHandlerForTestWithoutPublisher(t *testing.T, ctx context.Context, logger *zap.Logger, chains []vaa.ChainID) *mockData {
	md := mockData{}
	var err error

	md.sk, err = common.LoadGuardianKey("dev.guardian.key", true)
	require.NoError(t, err)
	require.NotNil(t, md.sk)

	ccqAllowedRequestersList, err := parseAllowedRequesters(testSigner)
	require.NoError(t, err)

	// Inbound observation requests from the p2p service (for all chains)
	md.signedQueryReqReadC, md.signedQueryReqWriteC = makeChannelPair[*gossipv1.SignedQueryRequest](SignedQueryRequestChannelSize)

	// Per-chain query requests
	md.chainQueryReqC = make(map[vaa.ChainID]chan *PerChainQueryInternal)
	for _, chainId := range chains {
		md.chainQueryReqC[chainId] = make(chan *PerChainQueryInternal)
	}

	// Query responses from watchers to query handler aggregated across all chains
	md.queryResponseReadC, md.queryResponseWriteC = makeChannelPair[*PerChainQueryResponseInternal](0)

	// Query responses from query handler to p2p
	md.queryResponsePublicationReadC, md.queryResponsePublicationWriteC = makeChannelPair[*QueryResponsePublication](0)

	md.resetState()

	go func() {
		err := handleQueryRequestsImpl(ctx, logger, md.signedQueryReqReadC, md.chainQueryReqC, ccqAllowedRequestersList,
			md.queryResponseReadC, md.queryResponsePublicationWriteC, common.GoTest, requestTimeoutForTest, retryIntervalForTest, auditIntervalForTest)
		assert.NoError(t, err)
	}()

	// Create a routine for each configured watcher. It will take a per chain query and return the corresponding expected result.
	// It also pegs a counter of the number of requests the watcher received, for verification purposes.
	for chainId := range md.chainQueryReqC {
		go func(chainId vaa.ChainID, chainQueryReqC <-chan *PerChainQueryInternal) {
			for {
				select {
				case <-ctx.Done():
					return
				case pcqr := <-chainQueryReqC:
					require.Equal(t, chainId, pcqr.Request.ChainId)
					md.mutex.Lock()
					md.incrementRequestsPerChainAlreadyLocked(chainId)
					if md.shouldIgnoreAlreadyLocked(chainId) {
						logger.Info("watcher ignoring query", zap.String("chainId", chainId.String()), zap.Int("requestIdx", pcqr.RequestIdx))
					} else {
						results := md.expectedResults[pcqr.RequestIdx].Response
						status := md.getStatusAlreadyLocked(chainId)
						logger.Info("watcher returning", zap.String("chainId", chainId.String()), zap.Int("requestIdx", pcqr.RequestIdx), zap.Int("status", int(status)))
						queryResponse := CreatePerChainQueryResponseInternal(pcqr.RequestID, pcqr.RequestIdx, pcqr.Request.ChainId, status, results)
						md.queryResponseWriteC <- queryResponse
					}
					md.mutex.Unlock()
				}
			}
		}(chainId, md.chainQueryReqC[chainId])
	}

	return &md
}

// startResponseListener starts the response listener routine. It is called as part of the standard mock environment set up. Or, it can be used
// along with `createQueryHandlerForTestWithoutPublisher“ to test retries of response publication (by delaying the start of the response listener).
func (md *mockData) startResponseListener(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case qrp := <-md.queryResponsePublicationReadC:
				md.mutex.Lock()
				md.queryResponsePublication = qrp
				md.mutex.Unlock()
			}
		}
	}()
}

// waitForResponse is used by the tests to wait for a response publication. It will eventually timeout if the query fails.
func (md *mockData) waitForResponse() *QueryResponsePublication {
	for count := 0; count < 50; count++ {
		time.Sleep(pollIntervalForTest)
		ret := md.getQueryResponsePublication()
		if ret != nil {
			return ret
		}
	}
	return nil
}

func TestPerChainConfigValid(t *testing.T) {
	for chainID, config := range perChainConfig {
		if config.NumWorkers <= 0 {
			assert.Equal(t, "", fmt.Sprintf(`perChainConfig for "%s" has an invalid NumWorkers: %d`, chainID.String(), config.NumWorkers))
		}
	}
}
