package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/elnosh/gonuts/cashu"
	"github.com/elnosh/gonuts/cashu/nuts/nut11"
	"github.com/elnosh/gonuts/wallet"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	decodepay "github.com/nbd-wtf/ln-decodepay"
)

// Custom Message Types
const (
	REBALANCE_REQUEST  = 42069
	REBALANCE_RESPONSE = 42070
	HTLC_SWAP          = 42071
)

type Message interface {
	CustomMessage(peer string) (lnrpc.SendCustomMessageRequest, error)
}

func processRebalanceAttempts(
	ctx context.Context,
	subClient lnrpc.Lightning_SubscribeCustomMessagesClient,
	lndClient *LndClient,
	cashuWallet *wallet.Wallet,
	rebalanceChan chan *rebalanceAttempt,
) {
	for {
		customMessageReceived, err := subClient.Recv()
		if err != nil {
			slog.Error(fmt.Sprintf("error receiving message from peer: %v", err))
			continue
		}
		peer := hex.EncodeToString(customMessageReceived.Peer)

		switch customMessageReceived.Type {
		case REBALANCE_REQUEST:
			var rebalanceRequest RebalanceRequest
			if err := json.Unmarshal(customMessageReceived.Data, &rebalanceRequest); err != nil {
				slog.Error(fmt.Sprintf("could not parse rebalance request message: %v", err))
				continue
			}
			slog.Info(fmt.Sprintf("received request to rebalance channel for %v sats", rebalanceRequest.Amount))

			if err := processRebalanceRequest(ctx, rebalanceRequest, peer, lndClient, cashuWallet); err != nil {
				slog.Error(err.Error())
			}

		case REBALANCE_RESPONSE:
			var rebalanceResponse RebalanceResponse
			if err := json.Unmarshal(customMessageReceived.Data, &rebalanceResponse); err != nil {
				slog.Error(fmt.Sprintf("could not parse rebalance response message: %v", err))
				continue
			}
			slog.Info(fmt.Sprintf("received rebalance response for %v sats", rebalanceResponse.EcashAmount))

			if err := processRebalanceResponse(ctx, rebalanceResponse, peer, lndClient, cashuWallet, rebalanceChan); err != nil {
				slog.Error(err.Error())
				pendingRebalanceAttempts.Delete(rebalanceResponse.Id)
			}

		case HTLC_SWAP:
			var swapMsg HTLCSwap
			if err := json.Unmarshal(customMessageReceived.Data, &swapMsg); err != nil {
				slog.Error(fmt.Sprintf("could not parse HTLC swap message: %v", err))
				continue
			}
			slog.Info("received HTLC swap message")

			if err := processHTLCSwap(ctx, swapMsg, lndClient, cashuWallet); err != nil {
				slog.Error(err.Error())
				pendingRebalanceAttempts.Delete(swapMsg.Id)
			}

		default:
			// ignore
			continue
		}
	}
}

type RebalanceRequest struct {
	Id string
	// amount to rebalance in channel
	Amount uint64
}

func (req RebalanceRequest) CustomMessage(peer string) (lnrpc.SendCustomMessageRequest, error) {
	peerBytes, err := hex.DecodeString(peer)
	if err != nil {
		return lnrpc.SendCustomMessageRequest{}, fmt.Errorf("invalid peer: %v", err)
	}

	messageData, err := json.Marshal(req)
	if err != nil {
		return lnrpc.SendCustomMessageRequest{}, err
	}

	return lnrpc.SendCustomMessageRequest{
		Peer: peerBytes,
		Type: REBALANCE_REQUEST,
		Data: messageData,
	}, nil
}

func processRebalanceRequest(
	ctx context.Context,
	request RebalanceRequest,
	peer string,
	lndClient *LndClient,
	cashuWallet *wallet.Wallet,
) error {
	// TODO: add fees that node wants to charge
	ecashAmount := request.Amount
	rebalanceResponse := RebalanceResponse{
		Id:          request.Id,
		PublicKey:   cashuWallet.GetReceivePubkey().SerializeCompressed(),
		Mints:       cashuWallet.TrustedMints(),
		EcashAmount: ecashAmount,
	}

	if err := lndClient.SendCustomMessage(ctx, rebalanceResponse, peer); err != nil {
		return fmt.Errorf("could not send rebalance response message: %v", err)
	}
	slog.Info(fmt.Sprintf("sent rebalance response for %v sats to peer", ecashAmount))

	rebalanceAttempt := rebalanceAttempt{
		id:          request.Id,
		publicKey:   cashuWallet.GetReceivePubkey().SerializeCompressed(),
		mints:       cashuWallet.TrustedMints(),
		ecashAmount: ecashAmount,
	}
	pendingRebalanceAttempts.Put(request.Id, rebalanceAttempt)

	return nil
}

type RebalanceResponse struct {
	Id string
	// public key to lock the ecash
	PublicKey []byte
	// list of mints willing to receive ecash from
	Mints []string
	// total amount to receive in ecash based on amount in request
	EcashAmount uint64
}

func (res RebalanceResponse) CustomMessage(peer string) (lnrpc.SendCustomMessageRequest, error) {
	peerBytes, err := hex.DecodeString(peer)
	if err != nil {
		return lnrpc.SendCustomMessageRequest{}, fmt.Errorf("invalid peer: %v", err)
	}

	messageData, err := json.Marshal(res)
	if err != nil {
		return lnrpc.SendCustomMessageRequest{}, err
	}

	return lnrpc.SendCustomMessageRequest{
		Peer: peerBytes,
		Type: REBALANCE_RESPONSE,
		Data: messageData,
	}, nil
}

func processRebalanceResponse(
	ctx context.Context,
	response RebalanceResponse,
	peer string,
	lndClient *LndClient,
	cashuWallet *wallet.Wallet,
	rebalanceChan chan *rebalanceAttempt,
) error {
	rebalance, ok := pendingRebalanceAttempts.Get(response.Id)
	if !ok {
		return fmt.Errorf("received 'REBALANCE_RESPONSE' message with unidentified id: %v ignoring...", response.Id)
	}

	// TODO: check willing to accept rebalanceResponse.EcashAmount

	// if don't have any proofs from specified trusted mints by peer, do swap first
	balanceByMints := cashuWallet.GetBalanceByMints()
	peerMints := response.Mints
	var targetMint string
	var amountToSwap uint64
	needSwap := true
	for _, m := range peerMints {
		targetMint = m
		if balanceByMints[m] > response.EcashAmount {
			needSwap = false
			break
		}
		amountToSwap = response.EcashAmount - balanceByMints[m]
	}

	if needSwap {
		var fromMint string
		hasBalanceForSwap := false
		for mint, mintBalance := range balanceByMints {
			if mintBalance > amountToSwap {
				hasBalanceForSwap = true
				fromMint = mint
			}
		}
		if hasBalanceForSwap {
			if _, ok := balanceByMints[targetMint]; !ok {
				if _, err := cashuWallet.AddMint(targetMint); err != nil {
					return err
				}
			}
			_, err := cashuWallet.MintSwap(amountToSwap, fromMint, targetMint)
			if err != nil {
				return err
			}
		} else {
			return fmt.Errorf("cashu wallet does not have enough funds for transaction")
		}
	}

	publicKey, err := secp256k1.ParsePubKey(response.PublicKey)
	if err != nil {
		return fmt.Errorf("got invalid public key in REBALANCE_RESPONSE message: %v", err)
	}

	preimage := make([]byte, 32)
	_, err = rand.Read(preimage)
	if err != nil {
		return fmt.Errorf("error generating random preimage: %v", err)
	}
	hexPreimage := hex.EncodeToString(preimage)

	addInvoiceRequest := lnrpc.Invoice{
		RPreimage: preimage,
		Value:     int64(rebalance.amount),
		Expiry:    60,
	}
	invoice, err := lndClient.grpcClient.AddInvoice(ctx, &addInvoiceRequest)
	if err != nil {
		return fmt.Errorf("could not create invoice for rebalance: %v", err)
	}

	expiry := time.Now().Add(time.Minute * 1)
	tags := nut11.P2PKTags{
		NSigs:    1,
		Pubkeys:  []*btcec.PublicKey{publicKey},
		Locktime: expiry.Add(time.Minute * 1).Unix(),
	}
	htlcLockedProofs, err := cashuWallet.HTLCLockedProofs(
		response.EcashAmount,
		targetMint,
		hexPreimage,
		&tags,
		true,
	)
	if err != nil {
		return fmt.Errorf("error generating HTLC locked proofs: %v", err)
	}

	ecashHTLC, err := cashu.NewTokenV4(htlcLockedProofs, cashuWallet.CurrentMint(), cashu.Sat, false)
	if err != nil {
		return fmt.Errorf("error creating ecash HTLC: %v", err)
	}
	cashuToken, err := ecashHTLC.Serialize()
	if err != nil {
		return fmt.Errorf("error creating ecash HTLC: %v", err)
	}

	htlcSwapMessage := HTLCSwap{
		Id:        rebalance.id,
		Invoice:   invoice.PaymentRequest,
		EcashHTLC: cashuToken,
	}
	if err := lndClient.SendCustomMessage(ctx, htlcSwapMessage, peer); err != nil {
		return fmt.Errorf("could not send HTLC Swap message: %v", err)
	}
	slog.Info("sent HTLC Swap message to peer")

	rebalance.publicKey = response.PublicKey
	rebalance.mints = response.Mints
	rebalance.ecashAmount = response.EcashAmount
	rebalance.invoice = invoice.PaymentRequest
	rebalance.ecashHTLC = cashuToken
	pendingRebalanceAttempts.Put(rebalance.id, rebalance)

	// subscribe to invoice changes
	go func() {
		subscribeInvoice := invoicesrpc.SubscribeSingleInvoiceRequest{
			RHash: invoice.RHash,
		}

		invoiceSubClient, err := lndClient.invoicesClient.SubscribeSingleInvoice(ctx, &subscribeInvoice)
		if err != nil {
			slog.Error(fmt.Sprintf("error setting invoice subscription: %v", err))
			rebalanceChan <- &rebalance
			return
		}

		for {
			invoiceChange, err := invoiceSubClient.Recv()
			if err != nil {
				slog.Error("could not receive invoice state change")
				continue
			}

			if invoiceChange.State == lnrpc.Invoice_SETTLED {
				logmsg := fmt.Sprintf("invoice %v for rebalancing channel was paid. Resuming intercepted HTLC", rebalance.invoice)
				slog.Info(logmsg)
				rebalance.rebalanceSucceeded = true
				rebalanceChan <- &rebalance
				break
			}
		}
	}()

	return nil
}

type HTLCSwap struct {
	Id      string
	Invoice string
	// ecash that will be unlocked after invoice is paid
	EcashHTLC string
}

func (s HTLCSwap) CustomMessage(peer string) (lnrpc.SendCustomMessageRequest, error) {
	peerBytes, err := hex.DecodeString(peer)
	if err != nil {
		return lnrpc.SendCustomMessageRequest{}, fmt.Errorf("invalid peer: %v", err)
	}

	messageData, err := json.Marshal(s)
	if err != nil {
		return lnrpc.SendCustomMessageRequest{}, err
	}

	return lnrpc.SendCustomMessageRequest{
		Peer: peerBytes,
		Type: HTLC_SWAP,
		Data: messageData,
	}, nil
}

func processHTLCSwap(
	ctx context.Context,
	swapMsg HTLCSwap,
	lndClient *LndClient,
	cashuWallet *wallet.Wallet,
) error {
	rebalance, ok := pendingRebalanceAttempts.Get(swapMsg.Id)
	if !ok {
		return fmt.Errorf("received 'HTLC_SWAP' message with unidentified id: %v ignoring...", swapMsg.Id)
	}

	bolt11, err := decodepay.Decodepay(swapMsg.Invoice)
	if err != nil {
		return fmt.Errorf("could not decode invoice in swap message: %v", err)
	}
	slog.Info(fmt.Sprintf("invoice is for '%v' sats", bolt11.MSatoshi/1000))

	token, err := cashu.DecodeToken(swapMsg.EcashHTLC)
	if err != nil {
		return fmt.Errorf("invalid cashu token in swap message: %v", err)
	}

	tokenMint := token.Mint()
	trustedMints := cashuWallet.TrustedMints()
	if !slices.Contains(trustedMints, tokenMint) {
		return errors.New("ecash is not from list of trusted mints")
	}

	proofs := token.Proofs()
	if err = VerifyEcashHTLC(proofs, bolt11, cashuWallet.GetReceivePubkey(), rebalance.ecashAmount); err != nil {
		return fmt.Errorf("could not verify ecash HTLC: %v", err)
	}

	// if ecash verification passed, pay invoice and redeem ecash
	sendPaymentReq := lnrpc.SendRequest{PaymentRequest: swapMsg.Invoice}
	sendPaymentRes, err := lndClient.grpcClient.SendPaymentSync(ctx, &sendPaymentReq)
	if err != nil {
		return fmt.Errorf("error paying invoice: %v", err)
	}
	if len(sendPaymentRes.PaymentError) > 0 {
		return fmt.Errorf("error paying invoice: %v", err)
	}

	preimage := hex.EncodeToString(sendPaymentRes.PaymentPreimage)
	ecashAmountReceived, err := cashuWallet.ReceiveHTLC(token, preimage)
	if err != nil {
		return fmt.Errorf("something bad happened, could not unlock redeem ecash HTLC: %v", err)
	}

	slog.Info(fmt.Sprintf("paid invoice '%v' and got %v sats in ecash", swapMsg.Invoice, ecashAmountReceived))
	pendingRebalanceAttempts.Delete(rebalance.id)

	return nil
}
