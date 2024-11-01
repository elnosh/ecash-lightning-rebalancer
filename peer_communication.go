package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/lightningnetwork/lnd/lnrpc"
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

type RebalanceRequest struct {
	Id string
	// amount to rebalance in channel
	Amount uint64
	// locktime
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
