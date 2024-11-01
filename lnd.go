package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"gopkg.in/macaroon.v2"
)

type LndConfig struct {
	GrpcHost     string
	TlsCertPath  string
	MacaroonPath string
}

type LndClient struct {
	grpcClient     lnrpc.LightningClient
	routerClient   routerrpc.RouterClient
	invoicesClient invoicesrpc.InvoicesClient
}

func SetupLndClient(config LndConfig) (*LndClient, error) {
	creds, err := credentials.NewClientTLSFromFile(config.TlsCertPath, "")
	if err != nil {
		return nil, err
	}

	macaroonBytes, err := os.ReadFile(config.MacaroonPath)
	if err != nil {
		return nil, fmt.Errorf("error reading macaroon: os.ReadFile %v", err)
	}

	macaroon := &macaroon.Macaroon{}
	if err = macaroon.UnmarshalBinary(macaroonBytes); err != nil {
		return nil, fmt.Errorf("unable to decode macaroon: %v", err)
	}
	macarooncreds, err := macaroons.NewMacaroonCredential(macaroon)
	if err != nil {
		return nil, fmt.Errorf("error setting macaroon creds: %v", err)
	}

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithPerRPCCredentials(macarooncreds),
	}

	conn, err := grpc.NewClient(config.GrpcHost, opts...)
	if err != nil {
		return nil, fmt.Errorf("error setting up grpc client: %v", err)
	}

	grpcClient := lnrpc.NewLightningClient(conn)
	routerClient := routerrpc.NewRouterClient(conn)
	invoicesClient := invoicesrpc.NewInvoicesClient(conn)
	return &LndClient{
		grpcClient:     grpcClient,
		routerClient:   routerClient,
		invoicesClient: invoicesClient,
	}, nil
}

func (lnd *LndClient) GetChannelById(ctx context.Context, chanId uint64) (*lnrpc.Channel, error) {
	listChannelsRequest := lnrpc.ListChannelsRequest{ActiveOnly: true}
	channelsList, err := lnd.grpcClient.ListChannels(ctx, &listChannelsRequest)
	if err != nil {
		return nil, err
	}

	for _, channel := range channelsList.Channels {
		if channel.ChanId == chanId {
			return channel, nil
		}
	}

	return nil, errors.New("channel not found")
}

func (lnd *LndClient) SendCustomMessage(ctx context.Context, msg Message, peer string) error {
	message, err := msg.CustomMessage(peer)
	if err != nil {
		return fmt.Errorf("could not build request message: %v", err)
	}

	_, err = lnd.grpcClient.SendCustomMessage(ctx, &message)
	if err != nil {
		return err
	}

	return nil
}
