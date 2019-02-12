package htlcswitch

import (
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/lnwallet"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"time"

	"encoding/hex"
	"github.com/lightningnetwork/lnd/channeldb"
	pb "github.com/lightningnetwork/lnd/lnrpc"
	"google.golang.org/grpc/credentials"
)

type HashResolverConfig struct {
	Active     bool   `long:"active" description:"whether the hash resolver is active"`
	ServerAddr string `long:"serveraddr" description:"host and port of the resolver"`
	TLS        bool   `long:"TLS" description:"If TLS should be used or not"`
	CaFile     string `long:"cafile" description:"The file containning the CA root cert file"`
}

type resolutionData struct {
	pd            *lnwallet.PaymentDescriptor
	l             *channelLink
	obfuscator    ErrorEncrypter
	preimageArray [32]byte
	failed        bool
}

func lookupResolverInvoice(cfg *HashResolverConfig, cltvDelta uint32, err error) (channeldb.Invoice, uint32, error) {
	invoice := channeldb.Invoice{}
	if !cfg.Active {
		log.Info("resolver is not active. Providing no invoice")
		return invoice, 0, err
	}
	invoice.Terms = channeldb.ContractTerm{
		Value:   0,
		Settled: false,
	}
	log.Infof("resolver is active. Providing an invoice so HTLC will be accepted."+
		"TimeLockDelta = %v", cltvDelta)

	return invoice, cltvDelta, nil
}

func connectResolver(cfg *HashResolverConfig) (*grpc.ClientConn, pb.HashResolverClient, error) {
	var opts []grpc.DialOption
	if cfg.TLS {
		creds, err := credentials.NewClientTLSFromFile(cfg.CaFile, "")
		if err != nil {
			err = errors.New("Failed to create TLS credentials from " + cfg.CaFile + " " + err.Error())
			log.Error(err)
			return nil, nil, err
		}
		opts = append(opts, grpc.WithTransportCredentials(creds))
	} else {
		opts = append(opts, grpc.WithInsecure())
	}
	conn, err := grpc.Dial(cfg.ServerAddr, opts...)
	if err != nil {
		log.Errorf("failed to dial: %v", err)
		return nil, nil, errors.New("failed to open connection to resolver")
	}

	return conn, pb.NewHashResolverClient(conn), nil
}

func queryPreImage(cfg *HashResolverConfig, pd *lnwallet.PaymentDescriptor, heightNow uint32) (*pb.ResolveResponse, error) {
	conn, client, err := connectResolver(cfg)
	if err != nil {
		return nil, err
	}

	defer conn.Close()

	log.Debugf("Getting pre-image for hash: %v %v for amount %v", pd.RHash, hex.EncodeToString(pd.RHash[:]), int64(pd.Amount))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp, err := client.ResolveHash(ctx, &pb.ResolveRequest{
		Hash:      hex.EncodeToString(pd.RHash[:]),
		Amount:    int64(pd.Amount),
		Timeout:   pd.Timeout,
		HeightNow: heightNow,
	})
	if err != nil {
		log.Errorf("%v.ResolveHash(_) = _, %v: ", client, err)
		return nil, err
	}
	log.Debugf("Got response from Resolver: %v \n", resp)
	return resp, nil
}

func asyncResolve(cfg *HashResolverConfig, pd *lnwallet.PaymentDescriptor, l *channelLink, obfuscator ErrorEncrypter, heightNow uint32) {
	go func() {
		// prepare message to main routine
		resolution := resolutionData{
			pd:         pd,
			l:          l,
			obfuscator: obfuscator,
		}

		resp, err := queryPreImage(cfg, pd, heightNow)

		if err != nil {
			log.Errorf("Error from queryPreImage: %v", err)
			resolution.failed = true
			l.resolver <- resolution
			return
		}

		// we got a pre-image. Try to decode it
		preimage, err := hex.DecodeString(resp.Preimage)
		if err != nil {
			log.Errorf("unable to decode Preimage %v : "+
				" %v", resp.Preimage, err)
			resolution.failed = true
			l.resolver <- resolution
			return
		}

		copy(resolution.preimageArray[:], preimage[:32])
		log.Debugf("preimage %v , resp.Preimage %v, preimageArray %v", preimage, resp.Preimage, resolution.preimageArray)
		resolution.failed = false
		l.resolver <- resolution
	}()
}
