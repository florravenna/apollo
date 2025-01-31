package libwallet

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/netann"
	"github.com/lightningnetwork/lnd/zpay32"

	"github.com/muun/libwallet/hdpath"
	"github.com/muun/libwallet/sphinx"
	"github.com/muun/libwallet/walletdb"
)

const MaxUnusedSecrets = 5

const (
	identityKeyChildIndex = 0
	htlcKeyChildIndex     = 1
)

// InvoiceSecrets represents a bundle of secrets required to generate invoices
// from the client. These secrets must be registered with the remote server
// and persisted in the client database before use.
type InvoiceSecrets struct {
	preimage      []byte
	paymentSecret []byte
	keyPath       string
	PaymentHash   []byte
	IdentityKey   *HDPublicKey
	UserHtlcKey   *HDPublicKey
	MuunHtlcKey   *HDPublicKey
	ShortChanId   int64
}

// RouteHints is a struct returned by the remote server containing the data
// necessary for constructing an invoice locally.
type RouteHints struct {
	Pubkey                    string
	FeeBaseMsat               int64
	FeeProportionalMillionths int64
	CltvExpiryDelta           int32
}

// InvoiceOptions defines additional options that can be configured when
// creating a new invoice.
type InvoiceOptions struct {
	Description string
	AmountSat   int64
}

// InvoiceSecretsList is a wrapper around an InvoiceSecrets slice to be
// able to pass through the gomobile bridge.
type InvoiceSecretsList struct {
	secrets []*InvoiceSecrets
}

// Length returns the number of secrets in the list.
func (l *InvoiceSecretsList) Length() int {
	return len(l.secrets)
}

// Get returns the secret at the given index.
func (l *InvoiceSecretsList) Get(i int) *InvoiceSecrets {
	return l.secrets[i]
}

// GenerateInvoiceSecrets returns a slice of new secrets to register with
// the remote server. Once registered, those invoices should be stored with
// the PersistInvoiceSecrets method.
func GenerateInvoiceSecrets(userKey, muunKey *HDPublicKey) (*InvoiceSecretsList, error) {

	var secrets []*InvoiceSecrets

	db, err := openDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	unused, err := db.CountUnusedInvoices()
	if err != nil {
		return nil, err
	}

	if unused >= MaxUnusedSecrets {
		return &InvoiceSecretsList{make([]*InvoiceSecrets, 0)}, nil
	}

	num := MaxUnusedSecrets - unused

	for i := 0; i < num; i++ {
		preimage := randomBytes(32)
		paymentSecret := randomBytes(32)
		paymentHashArray := sha256.Sum256(preimage)
		paymentHash := paymentHashArray[:]

		levels := randomBytes(8)
		l1 := binary.LittleEndian.Uint32(levels[:4]) & 0x7FFFFFFF
		l2 := binary.LittleEndian.Uint32(levels[4:]) & 0x7FFFFFFF

		keyPath := hdpath.MustParse("m/schema:1'/recovery:1'/invoices:4").Child(l1).Child(l2)

		identityKeyPath := keyPath.Child(identityKeyChildIndex)

		identityKey, err := userKey.DeriveTo(identityKeyPath.String())
		if err != nil {
			return nil, err
		}

		htlcKeyPath := keyPath.Child(htlcKeyChildIndex)

		userHtlcKey, err := userKey.DeriveTo(htlcKeyPath.String())
		if err != nil {
			return nil, err
		}
		muunHtlcKey, err := muunKey.DeriveTo(htlcKeyPath.String())
		if err != nil {
			return nil, err
		}

		shortChanId := binary.LittleEndian.Uint64(randomBytes(8)) | (1 << 63)

		secrets = append(secrets, &InvoiceSecrets{
			preimage:      preimage,
			paymentSecret: paymentSecret,
			keyPath:       keyPath.String(),
			PaymentHash:   paymentHash,
			IdentityKey:   identityKey,
			UserHtlcKey:   userHtlcKey,
			MuunHtlcKey:   muunHtlcKey,
			ShortChanId:   int64(shortChanId),
		})
	}

	// TODO: cleanup used secrets

	return &InvoiceSecretsList{secrets}, nil
}

// PersistInvoiceSecrets stores secrets registered with the remote server
// in the device local database. These secrets can be used to craft new
// Lightning invoices.
func PersistInvoiceSecrets(list *InvoiceSecretsList) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	for _, s := range list.secrets {
		db.CreateInvoice(&walletdb.Invoice{
			Preimage:      s.preimage,
			PaymentHash:   s.PaymentHash,
			PaymentSecret: s.paymentSecret,
			KeyPath:       s.keyPath,
			ShortChanId:   uint64(s.ShortChanId),
			State:         walletdb.InvoiceStateRegistered,
		})
	}
	return nil
}

// CreateInvoice returns a new lightning invoice string for the given network.
// Amount and description can be configured optionally.
func CreateInvoice(net *Network, userKey *HDPrivateKey, routeHints *RouteHints, opts *InvoiceOptions) (string, error) {
	// obtain first unused secret from db
	db, err := openDB()
	if err != nil {
		return "", err
	}
	defer db.Close()

	dbInvoice, err := db.FindFirstUnusedInvoice()
	if err != nil {
		return "", err
	}
	if dbInvoice == nil {
		return "", nil
	}

	var paymentHash [32]byte
	copy(paymentHash[:], dbInvoice.PaymentHash)

	nodeID, err := parsePubKey(routeHints.Pubkey)
	if err != nil {
		return "", fmt.Errorf("can't parse route hint pubkey: %w", err)
	}

	var iopts []func(*zpay32.Invoice)
	iopts = append(iopts, zpay32.RouteHint([]zpay32.HopHint{
		{
			NodeID:                    nodeID,
			ChannelID:                 dbInvoice.ShortChanId,
			FeeBaseMSat:               uint32(routeHints.FeeBaseMsat),
			FeeProportionalMillionths: uint32(routeHints.FeeProportionalMillionths),
			CLTVExpiryDelta:           uint16(routeHints.CltvExpiryDelta),
		},
	}))

	features := lnwire.EmptyFeatureVector()
	features.RawFeatureVector.Set(lnwire.TLVOnionPayloadOptional)
	features.RawFeatureVector.Set(lnwire.PaymentAddrOptional)

	iopts = append(iopts, zpay32.Features(features))
	iopts = append(iopts, zpay32.CLTVExpiry(72)) // ~1/2 day
	iopts = append(iopts, zpay32.Expiry(1*time.Hour))

	var paymentAddr [32]byte
	copy(paymentAddr[:], dbInvoice.PaymentSecret)
	iopts = append(iopts, zpay32.PaymentAddr(paymentAddr))

	if opts.Description != "" {
		iopts = append(iopts, zpay32.Description(opts.Description))
	} else {
		// description or description hash must be non-empty, adding a placeholder for now
		iopts = append(iopts, zpay32.Description(""))
	}
	if opts.AmountSat != 0 {
		msat := lnwire.NewMSatFromSatoshis(btcutil.Amount(opts.AmountSat))
		iopts = append(iopts, zpay32.Amount(msat))
	}

	// create the invoice
	invoice, err := zpay32.NewInvoice(
		net.network, paymentHash, time.Now(), iopts...,
	)
	if err != nil {
		return "", err
	}

	// recreate the client identity privkey
	identityKeyPath := hdpath.MustParse(dbInvoice.KeyPath).Child(identityKeyChildIndex)
	identityHDKey, err := userKey.DeriveTo(identityKeyPath.String())
	if err != nil {
		return "", err
	}
	identityKey, err := identityHDKey.key.ECPrivKey()
	if err != nil {
		return "", fmt.Errorf("can't obtain identity privkey: %w", err)
	}

	// sign the invoice with the identity pubkey
	signer := netann.NewNodeSigner(identityKey)
	bech32, err := invoice.Encode(zpay32.MessageSigner{
		SignCompact: signer.SignDigestCompact,
	})
	if err != nil {
		return "", err
	}

	now := time.Now()
	dbInvoice.AmountSat = opts.AmountSat
	dbInvoice.State = walletdb.InvoiceStateUsed
	dbInvoice.UsedAt = &now

	err = db.SaveInvoice(dbInvoice)
	if err != nil {
		return "", err
	}

	return bech32, nil
}

type IncomingSwap struct {
	Htlc             *IncomingSwapHtlc
	SphinxPacket     []byte
	PaymentHash      []byte
	PaymentAmountSat int64
	CollectSat       int64
}

type IncomingSwapHtlc struct {
	HtlcTx              []byte
	ExpirationHeight    int64
	SwapServerPublicKey []byte
}

type IncomingSwapFulfillmentData struct {
	FulfillmentTx      []byte
	MuunSignature      []byte
	OutputVersion      int    // unused
	OutputPath         string // unused
	MerkleTree         []byte // unused
	HtlcBlock          []byte // unused
	BlockHeight        int64  // unused
	ConfirmationTarget int64  // to validate fee rate, unused for now
}

type IncomingSwapFulfillmentResult struct {
	FulfillmentTx []byte
	Preimage      []byte
}

func (s *IncomingSwap) getInvoice() (*walletdb.Invoice, error) {
	db, err := openDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	return db.FindByPaymentHash(s.PaymentHash)
}

func (s *IncomingSwap) VerifyFulfillable(userKey *HDPrivateKey, net *Network) error {
	paymentHash := s.PaymentHash

	if len(paymentHash) != 32 {
		return fmt.Errorf("VerifyFulfillable: received invalid hash len %v", len(paymentHash))
	}

	// Lookup invoice data matching this HTLC using the payment hash
	invoice, err := s.getInvoice()
	if err != nil {
		return fmt.Errorf("VerifyFulfillable: could not find invoice data for payment hash: %w", err)
	}

	identityKeyPath := hdpath.MustParse(invoice.KeyPath).Child(identityKeyChildIndex)

	nodeHDKey, err := userKey.DeriveTo(identityKeyPath.String())
	if err != nil {
		return fmt.Errorf("VerifyFulfillable: failed to derive key: %w", err)
	}
	nodeKey, err := nodeHDKey.key.ECPrivKey()
	if err != nil {
		return fmt.Errorf("VerifyFulfillable: failed to get priv key: %w", err)
	}

	// implementation is allowed to send a few extra sats
	if invoice.AmountSat != 0 && invoice.AmountSat > s.PaymentAmountSat {
		return fmt.Errorf("VerifyFulfillable: payment amount (%v) does not match invoice amount (%v)",
			s.PaymentAmountSat, invoice.AmountSat)
	}

	if len(s.SphinxPacket) == 0 {
		return nil
	}

	err = sphinx.Validate(
		s.SphinxPacket,
		paymentHash,
		invoice.PaymentSecret,
		nodeKey,
		0, // This is used internally by the sphinx decoder but it's not needed
		lnwire.MilliSatoshi(uint64(s.PaymentAmountSat)*1000),
		net.network,
	)
	if err != nil {
		return fmt.Errorf("VerifyFulfillable: invalid sphinx: %w", err)
	}

	return nil
}

func (s *IncomingSwap) Fulfill(
	data *IncomingSwapFulfillmentData,
	userKey *HDPrivateKey, muunKey *HDPublicKey,
	net *Network) (*IncomingSwapFulfillmentResult, error) {

	if s.Htlc == nil {
		return nil, fmt.Errorf("Fulfill: missing swap htlc data")
	}

	err := s.VerifyFulfillable(userKey, net)
	if err != nil {
		return nil, err
	}

	// Validate the fullfillment tx proposed by Muun.
	tx := wire.MsgTx{}
	err = tx.DeserializeNoWitness(bytes.NewReader(data.FulfillmentTx))
	if err != nil {
		return nil, fmt.Errorf("Fulfill: could not deserialize fulfillment tx: %w", err)
	}
	if len(tx.TxIn) != 1 {
		return nil, fmt.Errorf("Fulfill: expected fulfillment tx to have exactly 1 input, found %d", len(tx.TxIn))
	}
	if len(tx.TxOut) != 1 {
		return nil, fmt.Errorf("Fulfill: expected fulfillment tx to have exactly 1 output, found %d", len(tx.TxOut))
	}

	// Lookup invoice data matching this HTLC using the payment hash
	invoice, err := s.getInvoice()
	if err != nil {
		return nil, fmt.Errorf("Fulfill: could not find invoice data for payment hash: %w", err)
	}

	// Sign the htlc input (there is only one, at index 0)
	coin := coinIncomingSwap{
		Network:             net.network,
		MuunSignature:       data.MuunSignature,
		Sphinx:              s.SphinxPacket,
		HtlcTx:              s.Htlc.HtlcTx,
		PaymentHash256:      s.PaymentHash,
		SwapServerPublicKey: []byte(s.Htlc.SwapServerPublicKey),
		ExpirationHeight:    s.Htlc.ExpirationHeight,
		VerifyOutputAmount:  true,
		Collect:             btcutil.Amount(s.CollectSat),
	}
	err = coin.SignInput(0, &tx, userKey, muunKey)
	if err != nil {
		return nil, err
	}

	// Serialize and return the signed fulfillment tx
	var buf bytes.Buffer
	err = tx.Serialize(&buf)
	if err != nil {
		return nil, fmt.Errorf("Fulfill: could not serialize fulfillment tx: %w", err)
	}
	return &IncomingSwapFulfillmentResult{
		FulfillmentTx: buf.Bytes(),
		Preimage:      invoice.Preimage,
	}, nil
}

// FulfillFullDebt gives the preimage matching a payment hash if we have it
func (s *IncomingSwap) FulfillFullDebt() (*IncomingSwapFulfillmentResult, error) {

	// Lookup invoice data matching this HTLC using the payment hash
	db, err := openDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	secrets, err := db.FindByPaymentHash(s.PaymentHash)
	if err != nil {
		return nil, fmt.Errorf("FulfillFullDebt: could not find invoice data for payment hash: %w", err)
	}

	return &IncomingSwapFulfillmentResult{
		FulfillmentTx: nil,
		Preimage:      secrets.Preimage,
	}, nil
}

func openDB() (*walletdb.DB, error) {
	return walletdb.Open(path.Join(cfg.DataDir, "wallet.db"))
}

func parsePubKey(s string) (*btcec.PublicKey, error) {
	bytes, err := hex.DecodeString(s)
	if err != nil {
		return nil, err
	}
	return btcec.ParsePubKey(bytes, btcec.S256())
}

func verifyTxWitnessSignature(tx *wire.MsgTx, sigHashes *txscript.TxSigHashes, outputIndex int, amount int64, script []byte, sig []byte, signKey *btcec.PublicKey) error {
	sigHash, err := txscript.CalcWitnessSigHash(script, sigHashes, txscript.SigHashAll, tx, outputIndex, amount)
	if err != nil {
		return err
	}
	signature, err := btcec.ParseDERSignature(sig, btcec.S256())
	if err != nil {
		return err
	}
	if !signature.Verify(sigHash, signKey) {
		return errors.New("signature does not verify")
	}
	return nil
}
