package ccq

import (
	"crypto/ecdsa"
	"fmt"
	"net/http"

	"github.com/certusone/wormhole/node/pkg/common"
	gossipv1 "github.com/certusone/wormhole/node/pkg/proto/gossip/v1"
	"github.com/certusone/wormhole/node/pkg/query"
	"github.com/wormhole-foundation/wormhole/sdk/vaa"
	"go.uber.org/zap"

	ethCrypto "github.com/ethereum/go-ethereum/crypto"

	"github.com/gagliardetto/solana-go"
)

func FetchCurrentGuardianSet(rpcUrl, coreAddr string) (*common.GuardianSet, error) {
	return nil, fmt.Errorf("not supported")
}

// validateRequest verifies that this API key is allowed to do all of the calls in this request. In the case of an error, it returns the HTTP status.
func validateRequest(logger *zap.Logger, env common.Environment, perms *Permissions, signerKey *ecdsa.PrivateKey, apiKey string, qr *gossipv1.SignedQueryRequest) (int, *query.QueryRequest, error) {
	permsForUser, exists := perms.GetUserEntry(apiKey)
	if !exists {
		logger.Debug("invalid api key", zap.String("apiKey", apiKey))
		invalidQueryRequestReceived.WithLabelValues("invalid_api_key").Inc()
		return http.StatusForbidden, nil, fmt.Errorf("invalid api key")
	}

	// TODO: Should we verify the signatures?

	if len(qr.Signature) == 0 {
		if !permsForUser.allowUnsigned || signerKey == nil {
			logger.Debug("request not signed and unsigned requests not supported for this user",
				zap.String("userName", permsForUser.userName),
				zap.Bool("allowUnsigned", permsForUser.allowUnsigned),
				zap.Bool("signerKeyConfigured", signerKey != nil),
			)
			invalidQueryRequestReceived.WithLabelValues("request_not_signed").Inc()
			return http.StatusBadRequest, nil, fmt.Errorf("request not signed")
		}

		// Sign the request using our key.
		var err error
		digest := query.QueryRequestDigest(env, qr.QueryRequest)
		qr.Signature, err = ethCrypto.Sign(digest.Bytes(), signerKey)
		if err != nil {
			logger.Debug("failed to sign request", zap.String("userName", permsForUser.userName), zap.Error(err))
			invalidQueryRequestReceived.WithLabelValues("failed_to_sign_request").Inc()
			return http.StatusInternalServerError, nil, fmt.Errorf("failed to sign request: %w", err)
		}
	}

	var queryRequest query.QueryRequest
	err := queryRequest.Unmarshal(qr.QueryRequest)
	if err != nil {
		logger.Debug("failed to unmarshal request", zap.String("userName", permsForUser.userName), zap.Error(err))
		invalidQueryRequestReceived.WithLabelValues("failed_to_unmarshal_request").Inc()
		return http.StatusBadRequest, nil, fmt.Errorf("failed to unmarshal request: %w", err)
	}

	// Make sure the overall query request is sane.
	if err := queryRequest.Validate(); err != nil {
		logger.Debug("failed to validate request", zap.String("userName", permsForUser.userName), zap.Error(err))
		invalidQueryRequestReceived.WithLabelValues("failed_to_validate_request").Inc()
		return http.StatusBadRequest, nil, fmt.Errorf("failed to validate request: %w", err)
	}

	// Make sure they are allowed to make all of the calls that they are asking for.
	for _, pcq := range queryRequest.PerChainQueries {
		var status int
		var err error
		switch q := pcq.Query.(type) {
		case *query.SolanaAccountQueryRequest:
			status, err = validateSolanaAccountQuery(logger, permsForUser, "solAccount", pcq.ChainId, q)
		case *query.SolanaPdaQueryRequest:
			status, err = validateSolanaPdaQuery(logger, permsForUser, "solPDA", pcq.ChainId, q)
		default:
			logger.Debug("unsupported query type", zap.String("userName", permsForUser.userName), zap.Any("type", pcq.Query))
			invalidQueryRequestReceived.WithLabelValues("unsupported_query_type").Inc()
			return http.StatusBadRequest, nil, fmt.Errorf("unsupported query type")
		}

		if err != nil {
			// Metric is pegged below.
			return status, nil, err
		}
	}

	logger.Debug("submitting query request", zap.String("userName", permsForUser.userName))
	return http.StatusOK, &queryRequest, nil
}

// validateSolanaAccountQuery performs verification on a Solana sol_account query.
func validateSolanaAccountQuery(logger *zap.Logger, permsForUser *permissionEntry, callTag string, chainId vaa.ChainID, q *query.SolanaAccountQueryRequest) (int, error) {
	for _, acct := range q.Accounts {
		callKey := fmt.Sprintf("%s:%d:%s", callTag, chainId, solana.PublicKey(acct).String())
		if _, exists := permsForUser.allowedCalls[callKey]; !exists {
			logger.Debug("requested call not authorized", zap.String("userName", permsForUser.userName), zap.String("callKey", callKey))
			invalidQueryRequestReceived.WithLabelValues("call_not_authorized").Inc()
			return http.StatusForbidden, fmt.Errorf(`call "%s" not authorized`, callKey)
		}

		totalRequestedCallsByChain.WithLabelValues(chainId.String()).Inc()
	}

	return http.StatusOK, nil
}

// validateSolanaPdaQuery performs verification on a Solana sol_account query.
func validateSolanaPdaQuery(logger *zap.Logger, permsForUser *permissionEntry, callTag string, chainId vaa.ChainID, q *query.SolanaPdaQueryRequest) (int, error) {
	for _, acct := range q.PDAs {
		callKey := fmt.Sprintf("%s:%d:%s", callTag, chainId, solana.PublicKey(acct.ProgramAddress).String())
		if _, exists := permsForUser.allowedCalls[callKey]; !exists {
			logger.Debug("requested call not authorized", zap.String("userName", permsForUser.userName), zap.String("callKey", callKey))
			invalidQueryRequestReceived.WithLabelValues("call_not_authorized").Inc()
			return http.StatusForbidden, fmt.Errorf(`call "%s" not authorized`, callKey)
		}

		totalRequestedCallsByChain.WithLabelValues(chainId.String()).Inc()
	}

	return http.StatusOK, nil
}
