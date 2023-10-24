// Copyright © 2023 Kaleido, Inc.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ethsigner

import (
	"context"
	"fmt"
	"math/big"
	"testing"

	"github.com/hyperledger/firefly-signer/mocks/secp256k1mocks"
	"github.com/hyperledger/firefly-signer/pkg/eip712"
	"github.com/hyperledger/firefly-signer/pkg/ethtypes"
	"github.com/hyperledger/firefly-signer/pkg/rlp"
	"github.com/hyperledger/firefly-signer/pkg/secp256k1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

func TestSignTypedDataV4(t *testing.T) {

	// We use a simple empty message payload
	payload := &eip712.TypedData{
		PrimaryType: eip712.EIP712Domain,
	}
	keypair, err := secp256k1.GenerateSecp256k1KeyPair()
	assert.NoError(t, err)

	ctx := context.Background()
	raw, err := SignTypedDataV4(ctx, keypair, payload)
	assert.NoError(t, err)

	rlpList, _, err := rlp.Decode(raw)
	assert.NoError(t, err)
	foundSig := &secp256k1.SignatureData{
		V: new(big.Int),
		R: new(big.Int),
		S: new(big.Int),
	}
	foundSig.R.SetBytes([]byte(rlpList.(rlp.List)[1].(rlp.Data)))
	foundSig.S.SetBytes([]byte(rlpList.(rlp.List)[2].(rlp.Data)))
	foundSig.V.SetBytes([]byte(rlpList.(rlp.List)[3].(rlp.Data)))

	signaturePayload := ethtypes.HexBytes0xPrefix(rlpList.(rlp.List)[0].(rlp.Data))
	addr, err := foundSig.Recover(signaturePayload, -1 /* chain id is in the domain (not applied EIP-155 style to the V value) */)
	assert.NoError(t, err)
	assert.Equal(t, keypair.Address.String(), addr.String())

	encoded, err := eip712.EncodeTypedDataV4(ctx, payload)
	assert.NoError(t, err)

	// Check all is as we expect
	assert.Equal(t, "0x8d4a3f4082945b7879e2b55f181c31a77c8c0a464b70669458abbaaf99de4c38", encoded.String())
	assert.Equal(t, "0x8d4a3f4082945b7879e2b55f181c31a77c8c0a464b70669458abbaaf99de4c38", signaturePayload.String())
}

func TestSignTypedDataV4BadPayload(t *testing.T) {

	payload := &eip712.TypedData{
		PrimaryType: "missing",
	}

	keypair, err := secp256k1.GenerateSecp256k1KeyPair()
	assert.NoError(t, err)

	ctx := context.Background()
	_, err = SignTypedDataV4(ctx, keypair, payload)
	assert.Regexp(t, "FF22078", err)
}

func TestSignTypedDataV4SignFail(t *testing.T) {

	payload := &eip712.TypedData{
		PrimaryType: eip712.EIP712Domain,
	}

	msn := &secp256k1mocks.Signer{}
	msn.On("Sign", mock.Anything).Return(nil, fmt.Errorf("pop"))

	ctx := context.Background()
	_, err := SignTypedDataV4(ctx, msn, payload)
	assert.Regexp(t, "pop", err)
}
