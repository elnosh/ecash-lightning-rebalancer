package main

import (
	"errors"
	"fmt"
	"reflect"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/elnosh/gonuts/cashu"
	"github.com/elnosh/gonuts/cashu/nuts/nut10"
	"github.com/elnosh/gonuts/cashu/nuts/nut11"
	decodepay "github.com/nbd-wtf/ln-decodepay"
)

func VerifyEcashHTLC(proofs cashu.Proofs, bolt11 decodepay.Bolt11, pubkey *secp256k1.PublicKey, wantAmount uint64) error {
	proofsAmount := proofs.Amount()
	if proofsAmount != wantAmount {
		return fmt.Errorf("got ecash for '%v' sats but wanted '%v'", proofsAmount, wantAmount)
	}

	if cashu.CheckDuplicateProofs(proofs) {
		return errors.New("duplicate proofs")
	}

	for _, proof := range proofs {
		secret, err := nut10.DeserializeSecret(proof.Secret)
		if err != nil {
			return fmt.Errorf("proof has invalid secret: %v", err)
		}

		if secret.Kind != nut10.HTLC {
			return errors.New("proof secret is not of kind HTLC")
		}

		// verify invoice and ecash are locked to same hash
		if secret.Data.Data != bolt11.PaymentHash {
			return errors.New("invoice and ecash in swap message are not locked to same hash")
		}

		tags, err := nut11.ParseP2PKTags(secret.Data.Tags)
		if err != nil {
			return fmt.Errorf("proof secret has invalid tags: %v", err)
		}

		if (int64(bolt11.Expiry) + 30) > tags.Locktime {
			return errors.New("invalid locktime")
		}

		if tags.NSigs != 1 {
			return errors.New("n_sigs tag in ecash is not 1")
		}

		// pubkeys tag should only have 1 key
		if len(tags.Pubkeys) != 1 {
			return errors.New("pubkeys tag in ecash does not have length of 1")
		}

		// verify ecash is locked to own cashu wallet pubkey
		if !reflect.DeepEqual(
			tags.Pubkeys[0].SerializeCompressed(),
			pubkey.SerializeCompressed(),
		) {
			return errors.New("proof is not locked to correct pubkey")
		}
	}

	return nil
}
