//nolint:unparam
package adminrpc

import (
	"context"
	"crypto/ecdsa"
	"testing"
	"time"

	nodev1 "github.com/certusone/wormhole/node/pkg/proto/node/v1"
	"github.com/ethereum/go-ethereum/common"
	ethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"
	"github.com/wormhole-foundation/wormhole/sdk/vaa"
	"go.uber.org/zap"
)

func generateGS(num int) (keys []*ecdsa.PrivateKey, addrs []common.Address) {
	for i := 0; i < num; i++ {
		key, err := ethcrypto.GenerateKey()
		if err != nil {
			panic(err)
		}
		keys = append(keys, key)
		addrs = append(addrs, ethcrypto.PubkeyToAddress(key.PublicKey))
	}
	return
}

func addrsToHexStrings(addrs []common.Address) (out []string) {
	for _, addr := range addrs {
		out = append(out, addr.String())
	}
	return
}

func generateMockVAA(gsIndex uint32, gsKeys []*ecdsa.PrivateKey) []byte {
	v := &vaa.VAA{
		Version:          1,
		GuardianSetIndex: gsIndex,
		Signatures:       nil,
		Timestamp:        time.Now(),
		Nonce:            3,
		Sequence:         79,
		ConsistencyLevel: 1,
		EmitterChain:     1,
		EmitterAddress:   vaa.Address{},
		Payload:          []byte("test"),
	}
	for i, key := range gsKeys {
		v.AddSignature(key, uint8(i))
	}

	vBytes, err := v.Marshal()
	if err != nil {
		panic(err)
	}
	return vBytes
}

func setupAdminServerForVAASigning(gsIndex uint32, gsAddrs []common.Address) *nodePrivilegedService {
	gk, err := ethcrypto.GenerateKey()
	if err != nil {
		panic(err)
	}

	return &nodePrivilegedService{
		db:              nil,
		injectC:         nil,
		obsvReqSendC:    nil,
		logger:          zap.L(),
		signedInC:       nil,
		governor:        nil,
		gk:              gk,
		guardianAddress: ethcrypto.PubkeyToAddress(gk.PublicKey),
	}
}

func TestSignExistingVAA_NoVAA(t *testing.T) {
	s := setupAdminServerForVAASigning(0, []common.Address{})

	_, err := s.SignExistingVAA(context.Background(), &nodev1.SignExistingVAARequest{
		Vaa:                 nil,
		NewGuardianAddrs:    nil,
		NewGuardianSetIndex: 0,
	})
	require.ErrorContains(t, err, "failed to unmarshal VAA")
}

func TestSignExistingVAA_NotGuardian(t *testing.T) {
	gsKeys, gsAddrs := generateGS(5)
	s := setupAdminServerForVAASigning(0, gsAddrs)

	v := generateMockVAA(0, gsKeys)

	_, err := s.SignExistingVAA(context.Background(), &nodev1.SignExistingVAARequest{
		Vaa:                 v,
		NewGuardianAddrs:    addrsToHexStrings(gsAddrs),
		NewGuardianSetIndex: 1,
	})
	require.ErrorContains(t, err, "local guardian is not a member of the new guardian set")
}

func TestSignExistingVAA_InvalidVAA(t *testing.T) {
	gsKeys, gsAddrs := generateGS(5)
	s := setupAdminServerForVAASigning(0, gsAddrs)

	v := generateMockVAA(0, gsKeys[:2])

	gsAddrs = append(gsAddrs, s.guardianAddress)
	_, err := s.SignExistingVAA(context.Background(), &nodev1.SignExistingVAARequest{
		Vaa:                 v,
		NewGuardianAddrs:    addrsToHexStrings(gsAddrs),
		NewGuardianSetIndex: 1,
	})
	require.ErrorContains(t, err, "failed to verify existing VAA")
}

func TestSignExistingVAA_DuplicateGuardian(t *testing.T) {
	gsKeys, gsAddrs := generateGS(5)
	s := setupAdminServerForVAASigning(0, gsAddrs)

	v := generateMockVAA(0, gsKeys)

	gsAddrs = append(gsAddrs, s.guardianAddress)
	gsAddrs = append(gsAddrs, s.guardianAddress)
	_, err := s.SignExistingVAA(context.Background(), &nodev1.SignExistingVAARequest{
		Vaa:                 v,
		NewGuardianAddrs:    addrsToHexStrings(gsAddrs),
		NewGuardianSetIndex: 1,
	})
	require.ErrorContains(t, err, "duplicate guardians in the guardian set")
}

func TestSignExistingVAA_AlreadyGuardian(t *testing.T) {
	gsKeys, gsAddrs := generateGS(5)
	s := setupAdminServerForVAASigning(0, gsAddrs)

	v := generateMockVAA(0, append(gsKeys, s.gk))

	gsAddrs = append(gsAddrs, s.guardianAddress)
	_, err := s.SignExistingVAA(context.Background(), &nodev1.SignExistingVAARequest{
		Vaa:                 v,
		NewGuardianAddrs:    addrsToHexStrings(gsAddrs),
		NewGuardianSetIndex: 1,
	})
	require.ErrorContains(t, err, "local guardian is already on the old set")
}

func TestSignExistingVAA_NotAFutureGuardian(t *testing.T) {
	gsKeys, gsAddrs := generateGS(5)
	s := setupAdminServerForVAASigning(0, gsAddrs)

	v := generateMockVAA(0, gsKeys)

	_, err := s.SignExistingVAA(context.Background(), &nodev1.SignExistingVAARequest{
		Vaa:                 v,
		NewGuardianAddrs:    addrsToHexStrings(gsAddrs),
		NewGuardianSetIndex: 1,
	})
	require.ErrorContains(t, err, "local guardian is not a member of the new guardian set")
}

func TestSignExistingVAA_CantReachQuorum(t *testing.T) {
	gsKeys, gsAddrs := generateGS(5)
	s := setupAdminServerForVAASigning(0, gsAddrs)

	v := generateMockVAA(0, gsKeys)

	gsAddrs = append(gsAddrs, s.guardianAddress)
	_, err := s.SignExistingVAA(context.Background(), &nodev1.SignExistingVAARequest{
		Vaa:                 v,
		NewGuardianAddrs:    addrsToHexStrings(append(gsAddrs, common.Address{0, 1}, common.Address{3, 1}, common.Address{8, 1})),
		NewGuardianSetIndex: 1,
	})
	require.ErrorContains(t, err, "cannot reach quorum on new guardian set with the local signature")
}

func TestSignExistingVAA_Valid(t *testing.T) {
	gsKeys, gsAddrs := generateGS(5)
	s := setupAdminServerForVAASigning(0, gsAddrs)

	v := generateMockVAA(0, gsKeys)

	gsAddrs = append(gsAddrs, s.guardianAddress)
	res, err := s.SignExistingVAA(context.Background(), &nodev1.SignExistingVAARequest{
		Vaa:                 v,
		NewGuardianAddrs:    addrsToHexStrings(gsAddrs),
		NewGuardianSetIndex: 1,
	})

	require.NoError(t, err)
	v2 := generateMockVAA(1, append(gsKeys, s.gk))
	require.Equal(t, v2, res.Vaa)
}
