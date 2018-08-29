
package htlcswitch

import (
	"os"
	"time"
	"path/filepath"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"github.com/jessevdk/go-flags"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/go-errors/errors"

	pb "github.com/ExchangeUnion/swap-resolver/swapresolver"
	"encoding/hex"
)

const (
	defaultConfigFilename     = "resolve.conf"
	defaultServerAddress      = "127.0.0.1:10000"
	defaultTls				  = false
)

type config struct {
	Tls bool `long:"tls" description:"If tls should be used or not"`
	CaFile string
	ServerAddr string `long:"serveraddr" description:"host and port of the resolver"`
	ServerHostOverride string
}

var (

	cfg				= &config{
		Tls: defaultTls,
		CaFile: "",
		ServerAddr: defaultServerAddress,
		ServerHostOverride: "",
	}

)

func isResolverActive() bool{
	// first see if we have a configuration file at the working directory. If
	// we miss that, the resolver is not active
	dir, err := os.Getwd()
	if err != nil {
		log.Errorf(err.Error())
		return false
	}

	defaultConfigFile := filepath.Join(dir,"..", defaultConfigFilename)

	// now, try to read the configration. If not valid, the resolver is not
	// active
	log.Debugf("reading configuration from %v",defaultConfigFile)

	err = flags.IniParse(defaultConfigFile,cfg)

	if err != nil {
		log.Errorf("failed to read resolver configuration file (%v) - %v", defaultConfigFile, err)
		return false
	}

	// if all is well - resolver is active
	return true
}


func connectResolver() (*grpc.ClientConn, pb.SwapResolverClient, error){
	var opts []grpc.DialOption
	// TODO: add TLS support
	if cfg.Tls {
		//if *caFile == "" {
		//	*caFile = testdata.Path("ca.pem")
		//}
	//	creds, err := credentials.NewClientTLSFromFile(*caFile, *serverHostOverride)
	//	if err != nil {
	//		log.Fatalf("Failed to create TLS credentials %v", err)
	//	}
	//	opts = append(opts, grpc.WithTransportCredentials(creds))
	} else {
	opts = append(opts, grpc.WithInsecure())
	}
	conn, err := grpc.Dial(cfg.ServerAddr, opts...)
	if err != nil {
		log.Errorf("fail to dial: %v", err)
		return nil,nil,errors.New("fail to open connection to resolver")
	}

	return conn, pb.NewSwapResolverClient(conn), nil
}

func queryPreImage(pd *lnwallet.PaymentDescriptor, heightNow uint32) (*pb.ResolveResponse, error){

	conn, client, err := connectResolver()
	if err != nil{
		return nil, err
	}

	defer conn.Close()

	log.Debugf("Getting pre-image for hash: %v %v for amount %v",pd.RHash, hex.EncodeToString(pd.RHash[:]),int64(pd.Amount))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// TODO: add additional attributes needed by the resolver. Like the amount
	// we got, CTLV, etc
	resp, err := client.ResolveHash(ctx, &pb.ResolveRequest{
		Hash: hex.EncodeToString(pd.RHash[:]),
		Amount: int64(pd.Amount),
		Timeout: pd.Timeout,
		Heightnow: heightNow,
	})
	if err != nil {
		log.Errorf("%v.ResolveHash(_) = _, %v: ", client, err)
		return nil, err
	}
	log.Debugf("Got response from Resolver: %v \n",resp)
	return resp, nil
}

func update(l *channelLink){
	if err := l.updateCommitTx(); err != nil {
		l.fail(LinkFailureError{code: ErrInternalError},
			"unable to update commitment: %v", err)
		return
	}
}

func asyncResolve(pd *lnwallet.PaymentDescriptor, l *channelLink, obfuscator ErrorEncrypter, heightNow uint32)  {

	go func (){

		// at end we have to update the link with success or failure
		defer  update(l)

		resp, err := queryPreImage(pd, heightNow)

		// if we got an error we fail the HTLC
		// TODO: provide the specific error back to HTLC source
		if err != nil{
			log.Errorf("unable to query invoice registry: "+
				" %v", err)
			failure := lnwire.FailUnknownPaymentHash{}
			l.sendHTLCError(
				pd.HtlcIndex, failure, obfuscator, pd.SourceRef,
			)
			return
		}

		// we got a pre-image
		preimage, err := hex.DecodeString(resp.Preimage);
		if err != nil{
			log.Errorf("unable to decode Preimage %v : "+
				" %v", resp.Preimage, err)
			failure := lnwire.FailUnknownPaymentHash{}
			l.sendHTLCError(
				pd.HtlcIndex, failure, obfuscator, pd.SourceRef,
			)
			return
		}
		var preimageArray [32]byte
		copy(preimageArray[:], preimage[:32])

		log.Debugf("preimage %v , resp.Preimage %v, preimageArray %v",preimage,resp.Preimage, preimageArray);

		err = l.channel.SettleHTLC(
			preimageArray, pd.HtlcIndex, pd.SourceRef, nil, nil,
		)
		// if there is an error, fail the link
		if err != nil {
			//l.fail(LinkFailureError{code: ErrInternalError},
			//	"unable to settle htlc: %v", err)
			//return
			log.Errorf("unable to query invoice registry: "+
				" %v", err)
			failure := lnwire.FailUnknownPaymentHash{}
			l.sendHTLCError(
				pd.HtlcIndex, failure, obfuscator, pd.SourceRef,
			)
			return
		}

		l.cfg.Peer.SendMessage(false, &lnwire.UpdateFulfillHTLC{
			ChanID:          l.ChanID(),
			ID:              pd.HtlcIndex,
			PaymentPreimage: preimageArray,
		})
	}()

}


