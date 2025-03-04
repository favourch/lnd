package invoicesrpc

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/netann"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/zpay32"
)

const (
	// DefaultInvoiceExpiry is the default invoice expiry for new MPP
	// invoices.
	DefaultInvoiceExpiry = 24 * time.Hour

	// DefaultAMPInvoiceExpiry is the default invoice expiry for new AMP
	// invoices.
	DefaultAMPInvoiceExpiry = 30 * 24 * time.Hour

	// hopHintFactor is factor by which we scale the total amount of
	// inbound capacity we want our hop hints to represent, allowing us to
	// have some leeway if peers go offline.
	hopHintFactor = 2
)

// AddInvoiceConfig contains dependencies for invoice creation.
type AddInvoiceConfig struct {
	// AddInvoice is called to add the invoice to the registry.
	AddInvoice func(invoice *channeldb.Invoice, paymentHash lntypes.Hash) (
		uint64, error)

	// IsChannelActive is used to generate valid hop hints.
	IsChannelActive func(chanID lnwire.ChannelID) bool

	// ChainParams are required to properly decode invoice payment requests
	// that are marshalled over rpc.
	ChainParams *chaincfg.Params

	// NodeSigner is an implementation of the MessageSigner implementation
	// that's backed by the identity private key of the running lnd node.
	NodeSigner *netann.NodeSigner

	// DefaultCLTVExpiry is the default invoice expiry if no values is
	// specified.
	DefaultCLTVExpiry uint32

	// ChanDB is a global boltdb instance which is needed to access the
	// channel graph.
	ChanDB *channeldb.ChannelStateDB

	// Graph holds a reference to the ChannelGraph database.
	Graph *channeldb.ChannelGraph

	// GenInvoiceFeatures returns a feature containing feature bits that
	// should be advertised on freshly generated invoices.
	GenInvoiceFeatures func() *lnwire.FeatureVector

	// GenAmpInvoiceFeatures returns a feature containing feature bits that
	// should be advertised on freshly generated AMP invoices.
	GenAmpInvoiceFeatures func() *lnwire.FeatureVector

	// GetAlias allows the peer's alias SCID to be retrieved for private
	// option_scid_alias channels.
	GetAlias func(lnwire.ChannelID) (lnwire.ShortChannelID, error)
}

// AddInvoiceData contains the required data to create a new invoice.
type AddInvoiceData struct {
	// An optional memo to attach along with the invoice. Used for record
	// keeping purposes for the invoice's creator, and will also be set in
	// the description field of the encoded payment request if the
	// description_hash field is not being used.
	Memo string

	// The preimage which will allow settling an incoming HTLC payable to
	// this preimage. If Preimage is set, Hash should be nil. If both
	// Preimage and Hash are nil, a random preimage is generated.
	Preimage *lntypes.Preimage

	// The hash of the preimage. If Hash is set, Preimage should be nil.
	// This condition indicates that we have a 'hold invoice' for which the
	// htlc will be accepted and held until the preimage becomes known.
	Hash *lntypes.Hash

	// The value of this invoice in millisatoshis.
	Value lnwire.MilliSatoshi

	// Hash (SHA-256) of a description of the payment. Used if the
	// description of payment (memo) is too long to naturally fit within the
	// description field of an encoded payment request.
	DescriptionHash []byte

	// Payment request expiry time in seconds. Default is 3600 (1 hour).
	Expiry int64

	// Fallback on-chain address.
	FallbackAddr string

	// Delta to use for the time-lock of the CLTV extended to the final hop.
	CltvExpiry uint64

	// Whether this invoice should include routing hints for private
	// channels.
	Private bool

	// HodlInvoice signals that this invoice shouldn't be settled
	// immediately upon receiving the payment.
	HodlInvoice bool

	// Amp signals whether or not to create an AMP invoice.
	//
	// NOTE: Preimage should always be set to nil when this value is true.
	Amp bool

	// RouteHints are optional route hints that can each be individually used
	// to assist in reaching the invoice's destination.
	RouteHints [][]zpay32.HopHint
}

// paymentHashAndPreimage returns the payment hash and preimage for this invoice
// depending on the configuration.
//
// For AMP invoices (when Amp flag is true), this method always returns a nil
// preimage. The hash value can be set externally by the user using the Hash
// field, or one will be generated randomly. The payment hash here only serves
// as a unique identifier for insertion into the invoice index, as there is
// no universal preimage for an AMP payment.
//
// For MPP invoices (when Amp flag is false), this method may return nil
// preimage when create a hodl invoice, but otherwise will always return a
// non-nil preimage and the corresponding payment hash. The valid combinations
// are parsed as follows:
//   - Preimage == nil && Hash == nil -> (random preimage, H(random preimage))
//   - Preimage != nil && Hash == nil -> (Preimage, H(Preimage))
//   - Preimage == nil && Hash != nil -> (nil, Hash)
func (d *AddInvoiceData) paymentHashAndPreimage() (
	*lntypes.Preimage, lntypes.Hash, error) {

	if d.Amp {
		return d.ampPaymentHashAndPreimage()
	}

	return d.mppPaymentHashAndPreimage()
}

// ampPaymentHashAndPreimage returns the payment hash to use for an AMP invoice.
// The preimage will always be nil.
func (d *AddInvoiceData) ampPaymentHashAndPreimage() (*lntypes.Preimage, lntypes.Hash, error) {
	switch {
	// Preimages cannot be set on AMP invoice.
	case d.Preimage != nil:
		return nil, lntypes.Hash{},
			errors.New("preimage set on AMP invoice")

	// If a specific hash was requested, use that.
	case d.Hash != nil:
		return nil, *d.Hash, nil

	// Otherwise generate a random hash value, just needs to be unique to be
	// added to the invoice index.
	default:
		var paymentHash lntypes.Hash
		if _, err := rand.Read(paymentHash[:]); err != nil {
			return nil, lntypes.Hash{}, err
		}

		return nil, paymentHash, nil
	}
}

// mppPaymentHashAndPreimage returns the payment hash and preimage to use for an
// MPP invoice.
func (d *AddInvoiceData) mppPaymentHashAndPreimage() (*lntypes.Preimage, lntypes.Hash, error) {
	var (
		paymentPreimage *lntypes.Preimage
		paymentHash     lntypes.Hash
	)

	switch {

	// Only either preimage or hash can be set.
	case d.Preimage != nil && d.Hash != nil:
		return nil, lntypes.Hash{},
			errors.New("preimage and hash both set")

	// If no hash or preimage is given, generate a random preimage.
	case d.Preimage == nil && d.Hash == nil:
		paymentPreimage = &lntypes.Preimage{}
		if _, err := rand.Read(paymentPreimage[:]); err != nil {
			return nil, lntypes.Hash{}, err
		}
		paymentHash = paymentPreimage.Hash()

	// If just a hash is given, we create a hold invoice by setting the
	// preimage to unknown.
	case d.Preimage == nil && d.Hash != nil:
		paymentHash = *d.Hash

	// A specific preimage was supplied. Use that for the invoice.
	case d.Preimage != nil && d.Hash == nil:
		preimage := *d.Preimage
		paymentPreimage = &preimage
		paymentHash = d.Preimage.Hash()
	}

	return paymentPreimage, paymentHash, nil
}

// AddInvoice attempts to add a new invoice to the invoice database. Any
// duplicated invoices are rejected, therefore all invoices *must* have a
// unique payment preimage.
func AddInvoice(ctx context.Context, cfg *AddInvoiceConfig,
	invoice *AddInvoiceData) (*lntypes.Hash, *channeldb.Invoice, error) {

	paymentPreimage, paymentHash, err := invoice.paymentHashAndPreimage()
	if err != nil {
		return nil, nil, err
	}

	// The size of the memo, receipt and description hash attached must not
	// exceed the maximum values for either of the fields.
	if len(invoice.Memo) > channeldb.MaxMemoSize {
		return nil, nil, fmt.Errorf("memo too large: %v bytes "+
			"(maxsize=%v)", len(invoice.Memo), channeldb.MaxMemoSize)
	}
	if len(invoice.DescriptionHash) > 0 && len(invoice.DescriptionHash) != 32 {
		return nil, nil, fmt.Errorf("description hash is %v bytes, must be 32",
			len(invoice.DescriptionHash))
	}

	// We set the max invoice amount to 100k BTC, which itself is several
	// multiples off the current block reward.
	maxInvoiceAmt := btcutil.Amount(btcutil.SatoshiPerBitcoin * 100000)

	switch {
	// The value of the invoice must not be negative.
	case int64(invoice.Value) < 0:
		return nil, nil, fmt.Errorf("payments of negative value "+
			"are not allowed, value is %v", int64(invoice.Value))

	// Also ensure that the invoice is actually realistic, while preventing
	// any issues due to underflow.
	case invoice.Value.ToSatoshis() > maxInvoiceAmt:
		return nil, nil, fmt.Errorf("invoice amount %v is "+
			"too large, max is %v", invoice.Value.ToSatoshis(),
			maxInvoiceAmt)
	}

	amtMSat := invoice.Value

	// We also create an encoded payment request which allows the
	// caller to compactly send the invoice to the payer. We'll create a
	// list of options to be added to the encoded payment request. For now
	// we only support the required fields description/description_hash,
	// expiry, fallback address, and the amount field.
	var options []func(*zpay32.Invoice)

	// We only include the amount in the invoice if it is greater than 0.
	// By not including the amount, we enable the creation of invoices that
	// allow the payee to specify the amount of satoshis they wish to send.
	if amtMSat > 0 {
		options = append(options, zpay32.Amount(amtMSat))
	}

	// If specified, add a fallback address to the payment request.
	if len(invoice.FallbackAddr) > 0 {
		addr, err := btcutil.DecodeAddress(invoice.FallbackAddr,
			cfg.ChainParams)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid fallback address: %v",
				err)
		}
		options = append(options, zpay32.FallbackAddr(addr))
	}

	switch {
	// If expiry is set, specify it. If it is not provided, no expiry time
	// will be explicitly added to this payment request, which will imply
	// the default 3600 seconds.
	case invoice.Expiry > 0:

		// We'll ensure that the specified expiry is restricted to sane
		// number of seconds. As a result, we'll reject an invoice with
		// an expiry greater than 1 year.
		maxExpiry := time.Hour * 24 * 365
		expSeconds := invoice.Expiry

		if float64(expSeconds) > maxExpiry.Seconds() {
			return nil, nil, fmt.Errorf("expiry of %v seconds "+
				"greater than max expiry of %v seconds",
				float64(expSeconds), maxExpiry.Seconds())
		}

		expiry := time.Duration(invoice.Expiry) * time.Second
		options = append(options, zpay32.Expiry(expiry))

	// If no custom expiry is provided, use the default MPP expiry.
	case !invoice.Amp:
		options = append(options, zpay32.Expiry(DefaultInvoiceExpiry))

	// Otherwise, use the default AMP expiry.
	default:
		options = append(options, zpay32.Expiry(DefaultAMPInvoiceExpiry))
	}

	// If the description hash is set, then we add it do the list of options.
	// If not, use the memo field as the payment request description.
	if len(invoice.DescriptionHash) > 0 {
		var descHash [32]byte
		copy(descHash[:], invoice.DescriptionHash[:])
		options = append(options, zpay32.DescriptionHash(descHash))
	} else {
		// Use the memo field as the description. If this is not set
		// this will just be an empty string.
		options = append(options, zpay32.Description(invoice.Memo))
	}

	// We'll use our current default CLTV value unless one was specified as
	// an option on the command line when creating an invoice.
	switch {
	case invoice.CltvExpiry > math.MaxUint16:
		return nil, nil, fmt.Errorf("CLTV delta of %v is too large, max "+
			"accepted is: %v", invoice.CltvExpiry, math.MaxUint16)
	case invoice.CltvExpiry != 0:
		// Disallow user-chosen final CLTV deltas below the required
		// minimum.
		if invoice.CltvExpiry < routing.MinCLTVDelta {
			return nil, nil, fmt.Errorf("CLTV delta of %v must be "+
				"greater than minimum of %v",
				routing.MinCLTVDelta, invoice.CltvExpiry)
		}

		options = append(options,
			zpay32.CLTVExpiry(invoice.CltvExpiry))
	default:
		// TODO(roasbeef): assumes set delta between versions
		defaultDelta := cfg.DefaultCLTVExpiry
		options = append(options, zpay32.CLTVExpiry(uint64(defaultDelta)))
	}

	// We make sure that the given invoice routing hints number is within the
	// valid range
	if len(invoice.RouteHints) > 20 {
		return nil, nil, fmt.Errorf("number of routing hints must not exceed " +
			"maximum of 20")
	}

	// We continue by populating the requested routing hints indexing their
	// corresponding channels so we won't duplicate them.
	forcedHints := make(map[uint64]struct{})
	for _, h := range invoice.RouteHints {
		if len(h) == 0 {
			return nil, nil, fmt.Errorf("number of hop hint within a route must " +
				"be positive")
		}
		options = append(options, zpay32.RouteHint(h))

		// Only this first hop is our direct channel.
		forcedHints[h[0].ChannelID] = struct{}{}
	}

	// If we were requested to include routing hints in the invoice, then
	// we'll fetch all of our available private channels and create routing
	// hints for them.
	if invoice.Private {
		openChannels, err := cfg.ChanDB.FetchAllChannels()
		if err != nil {
			return nil, nil, fmt.Errorf("could not fetch all channels")
		}

		if len(openChannels) > 0 {
			// We filter the channels by excluding the ones that were specified by
			// the caller and were already added.
			var filteredChannels []*HopHintInfo
			for _, c := range openChannels {
				if _, ok := forcedHints[c.ShortChanID().ToUint64()]; ok {
					continue
				}

				// If this is a zero-conf channel, check if the
				// confirmed SCID was used in forcedHints.
				realScid := c.ZeroConfRealScid().ToUint64()
				if c.IsZeroConf() {
					if _, ok := forcedHints[realScid]; ok {
						continue
					}
				}

				chanID := lnwire.NewChanIDFromOutPoint(
					&c.FundingOutpoint,
				)

				// Check whether the the peer's alias was
				// provided in forcedHints.
				peerAlias, _ := cfg.GetAlias(chanID)
				peerScid := peerAlias.ToUint64()
				if _, ok := forcedHints[peerScid]; ok {
					continue
				}

				isActive := cfg.IsChannelActive(chanID)

				hopHintInfo := newHopHintInfo(c, isActive)
				filteredChannels = append(
					filteredChannels, hopHintInfo,
				)
			}

			// We'll restrict the number of individual route hints
			// to 20 to avoid creating overly large invoices.
			numMaxHophints := 20 - len(forcedHints)

			hopHintsCfg := newSelectHopHintsCfg(cfg)
			hopHints := SelectHopHints(
				amtMSat, hopHintsCfg, filteredChannels,
				numMaxHophints,
			)

			// Convert our set of selected hop hints into route
			// hints and add to our invoice options.
			for _, hopHint := range hopHints {
				routeHint := zpay32.RouteHint(hopHint)

				options = append(
					options, routeHint,
				)
			}
		}
	}

	// Set our desired invoice features and add them to our list of options.
	var invoiceFeatures *lnwire.FeatureVector
	if invoice.Amp {
		invoiceFeatures = cfg.GenAmpInvoiceFeatures()
	} else {
		invoiceFeatures = cfg.GenInvoiceFeatures()
	}
	options = append(options, zpay32.Features(invoiceFeatures))

	// Generate and set a random payment address for this invoice. If the
	// sender understands payment addresses, this can be used to avoid
	// intermediaries probing the receiver.
	var paymentAddr [32]byte
	if _, err := rand.Read(paymentAddr[:]); err != nil {
		return nil, nil, err
	}
	options = append(options, zpay32.PaymentAddr(paymentAddr))

	// Create and encode the payment request as a bech32 (zpay32) string.
	creationDate := time.Now()
	payReq, err := zpay32.NewInvoice(
		cfg.ChainParams, paymentHash, creationDate, options...,
	)
	if err != nil {
		return nil, nil, err
	}

	payReqString, err := payReq.Encode(zpay32.MessageSigner{
		SignCompact: func(msg []byte) ([]byte, error) {
			return cfg.NodeSigner.SignMessageCompact(msg, false)
		},
	})
	if err != nil {
		return nil, nil, err
	}

	newInvoice := &channeldb.Invoice{
		CreationDate:   creationDate,
		Memo:           []byte(invoice.Memo),
		PaymentRequest: []byte(payReqString),
		Terms: channeldb.ContractTerm{
			FinalCltvDelta:  int32(payReq.MinFinalCLTVExpiry()),
			Expiry:          payReq.Expiry(),
			Value:           amtMSat,
			PaymentPreimage: paymentPreimage,
			PaymentAddr:     paymentAddr,
			Features:        invoiceFeatures,
		},
		HodlInvoice: invoice.HodlInvoice,
	}

	log.Tracef("[addinvoice] adding new invoice %v",
		newLogClosure(func() string {
			return spew.Sdump(newInvoice)
		}),
	)

	// With all sanity checks passed, write the invoice to the database.
	_, err = cfg.AddInvoice(newInvoice, paymentHash)
	if err != nil {
		return nil, nil, err
	}

	return &paymentHash, newInvoice, nil
}

// chanCanBeHopHint returns true if the target channel is eligible to be a hop
// hint.
func chanCanBeHopHint(channel *HopHintInfo, cfg *SelectHopHintsCfg) (
	*channeldb.ChannelEdgePolicy, bool) {

	// Since we're only interested in our private channels, we'll skip
	// public ones.
	if channel.IsPublic {
		return nil, false
	}

	// Make sure the channel is active.
	if !channel.IsActive {
		log.Debugf("Skipping channel %v due to not "+
			"being eligible to forward payments",
			channel.ShortChannelID)
		return nil, false
	}

	// To ensure we don't leak unadvertised nodes, we'll make sure our
	// counterparty is publicly advertised within the network.  Otherwise,
	// we'll end up leaking information about nodes that intend to stay
	// unadvertised, like in the case of a node only having private
	// channels.
	var remotePub [33]byte
	copy(remotePub[:], channel.RemotePubkey.SerializeCompressed())
	isRemoteNodePublic, err := cfg.IsPublicNode(remotePub)
	if err != nil {
		log.Errorf("Unable to determine if node %x "+
			"is advertised: %v", remotePub, err)
		return nil, false
	}

	if !isRemoteNodePublic {
		log.Debugf("Skipping channel %v due to "+
			"counterparty %x being unadvertised",
			channel.ShortChannelID, remotePub)
		return nil, false
	}

	// Fetch the policies for each end of the channel.
	info, p1, p2, err := cfg.FetchChannelEdgesByID(channel.ShortChannelID)
	if err != nil {
		// In the case of zero-conf channels, it may be the case that
		// the alias SCID was deleted from the graph, and replaced by
		// the confirmed SCID. Check the Graph for the confirmed SCID.
		confirmedScid := channel.ConfirmedScidZC
		info, p1, p2, err = cfg.FetchChannelEdgesByID(confirmedScid)
		if err != nil {
			log.Errorf("Unable to fetch the routing policies for "+
				"the edges of the channel %v: %v",
				channel.ShortChannelID, err)
			return nil, false
		}
	}

	// Now, we'll need to determine which is the correct policy for HTLCs
	// being sent from the remote node.
	var remotePolicy *channeldb.ChannelEdgePolicy
	if bytes.Equal(remotePub[:], info.NodeKey1Bytes[:]) {
		remotePolicy = p1
	} else {
		remotePolicy = p2
	}

	return remotePolicy, true
}

// addHopHint creates a hop hint out of the passed channel and channel policy.
// The new hop hint is appended to the passed slice.
func addHopHint(hopHints *[][]zpay32.HopHint,
	channel *HopHintInfo, chanPolicy *channeldb.ChannelEdgePolicy,
	aliasScid lnwire.ShortChannelID) {

	hopHint := zpay32.HopHint{
		NodeID:      channel.RemotePubkey,
		ChannelID:   channel.ShortChannelID,
		FeeBaseMSat: uint32(chanPolicy.FeeBaseMSat),
		FeeProportionalMillionths: uint32(
			chanPolicy.FeeProportionalMillionths,
		),
		CLTVExpiryDelta: chanPolicy.TimeLockDelta,
	}

	var defaultScid lnwire.ShortChannelID
	if aliasScid != defaultScid {
		hopHint.ChannelID = aliasScid.ToUint64()
	}

	*hopHints = append(*hopHints, []zpay32.HopHint{hopHint})
}

// HopHintInfo contains the channel information required to create a hop hint.
type HopHintInfo struct {
	// IsPublic indicates whether a channel is advertised to the network.
	IsPublic bool

	// IsActive indicates whether the channel is online and available for
	// use.
	IsActive bool

	// FundingOutpoint is the funding txid:index for the channel.
	FundingOutpoint wire.OutPoint

	// RemotePubkey is the public key of the remote party that this channel
	// is in.
	RemotePubkey *btcec.PublicKey

	// RemoteBalance is the remote party's balance (our current incoming
	// capacity).
	RemoteBalance lnwire.MilliSatoshi

	// ShortChannelID is the short channel ID of the channel.
	ShortChannelID uint64

	// ConfirmedScidZC is the confirmed SCID of a zero-conf channel. This
	// may be used for looking up a channel in the graph.
	ConfirmedScidZC uint64

	// ScidAliasFeature denotes whether the channel has negotiated the
	// option-scid-alias feature bit.
	ScidAliasFeature bool
}

func newHopHintInfo(c *channeldb.OpenChannel, isActive bool) *HopHintInfo {
	isPublic := c.ChannelFlags&lnwire.FFAnnounceChannel != 0

	return &HopHintInfo{
		IsPublic:         isPublic,
		IsActive:         isActive,
		FundingOutpoint:  c.FundingOutpoint,
		RemotePubkey:     c.IdentityPub,
		RemoteBalance:    c.LocalCommitment.RemoteBalance,
		ShortChannelID:   c.ShortChannelID.ToUint64(),
		ConfirmedScidZC:  c.ZeroConfRealScid().ToUint64(),
		ScidAliasFeature: c.ChanType.HasScidAliasFeature(),
	}
}

// SelectHopHintsCfg contains the dependencies required to obtain hop hints
// for an invoice.
type SelectHopHintsCfg struct {
	// IsPublicNode is returns a bool indicating whether the node with the
	// given public key is seen as a public node in the graph from the
	// graph's source node's point of view.
	IsPublicNode func(pubKey [33]byte) (bool, error)

	// FetchChannelEdgesByID attempts to lookup the two directed edges for
	// the channel identified by the channel ID.
	FetchChannelEdgesByID func(chanID uint64) (*channeldb.ChannelEdgeInfo,
		*channeldb.ChannelEdgePolicy, *channeldb.ChannelEdgePolicy,
		error)

	// GetAlias allows the peer's alias SCID to be retrieved for private
	// option_scid_alias channels.
	GetAlias func(lnwire.ChannelID) (lnwire.ShortChannelID, error)
}

func newSelectHopHintsCfg(invoicesCfg *AddInvoiceConfig) *SelectHopHintsCfg {
	return &SelectHopHintsCfg{
		IsPublicNode:          invoicesCfg.Graph.IsPublicNode,
		FetchChannelEdgesByID: invoicesCfg.Graph.FetchChannelEdgesByID,
		GetAlias:              invoicesCfg.GetAlias,
	}
}

// sufficientHints checks whether we have sufficient hop hints, based on the
// following criteria:
// - Hop hint count: limit to a set number of hop hints, regardless of whether
//   we've reached our invoice amount or not.
// - Total incoming capacity: limit to our invoice amount * scaling factor to
//   allow for some of our links going offline.
//
// We limit our number of hop hints like this to keep our invoice size down,
// and to avoid leaking all our private channels when we don't need to.
func sufficientHints(numHints, maxHints, scalingFactor int, amount,
	totalHintAmount lnwire.MilliSatoshi) bool {

	if numHints >= maxHints {
		log.Debug("Reached maximum number of hop hints")
		return true
	}

	requiredAmount := amount * lnwire.MilliSatoshi(scalingFactor)
	if totalHintAmount >= requiredAmount {
		log.Debugf("Total hint amount: %v has reached target hint "+
			"bandwidth: %v (invoice amount: %v * factor: %v)",
			totalHintAmount, requiredAmount, amount,
			scalingFactor)

		return true
	}

	return false
}

// SelectHopHints will select up to numMaxHophints from the set of passed open
// channels. The set of hop hints will be returned as a slice of functional
// options that'll append the route hint to the set of all route hints.
//
// TODO(roasbeef): do proper sub-set sum max hints usually << numChans.
func SelectHopHints(amtMSat lnwire.MilliSatoshi, cfg *SelectHopHintsCfg,
	openChannels []*HopHintInfo,
	numMaxHophints int) [][]zpay32.HopHint {

	// We'll add our hop hints in two passes, first we'll add all channels
	// that are eligible to be hop hints, and also have a local balance
	// above the payment amount.
	var totalHintBandwidth lnwire.MilliSatoshi
	hopHintChans := make(map[wire.OutPoint]struct{})
	hopHints := make([][]zpay32.HopHint, 0, numMaxHophints)
	for _, channel := range openChannels {
		enoughHopHints := sufficientHints(
			len(hopHints), numMaxHophints, hopHintFactor, amtMSat,
			totalHintBandwidth,
		)
		if enoughHopHints {
			log.Debugf("First pass of hop selection has " +
				"sufficient hints")

			return hopHints
		}

		// If this channel can't be a hop hint, then skip it.
		edgePolicy, canBeHopHint := chanCanBeHopHint(channel, cfg)
		if edgePolicy == nil || !canBeHopHint {
			continue
		}

		// Similarly, in this first pass, we'll ignore all channels in
		// isolation can't satisfy this payment.
		if channel.RemoteBalance < amtMSat {
			continue
		}

		// Lookup and see if there is an alias SCID that exists.
		chanID := lnwire.NewChanIDFromOutPoint(
			&channel.FundingOutpoint,
		)
		alias, _ := cfg.GetAlias(chanID)

		// If this is a channel where the option-scid-alias feature bit
		// was negotiated and the alias is not yet assigned, we cannot
		// issue an invoice. Doing so might expose the confirmed SCID
		// of a private channel.
		if channel.ScidAliasFeature {
			var defaultScid lnwire.ShortChannelID
			if alias == defaultScid {
				continue
			}
		}

		// Now that we now this channel use usable, add it as a hop
		// hint and the indexes we'll use later.
		addHopHint(&hopHints, channel, edgePolicy, alias)

		hopHintChans[channel.FundingOutpoint] = struct{}{}
		totalHintBandwidth += channel.RemoteBalance
	}

	// In this second pass we'll add channels, and we'll either stop when
	// we have 20 hop hints, we've run through all the available channels,
	// or if the sum of available bandwidth in the routing hints exceeds 2x
	// the payment amount. We do 2x here to account for a margin of error
	// if some of the selected channels no longer become operable.
	for i := 0; i < len(openChannels); i++ {
		enoughHopHints := sufficientHints(
			len(hopHints), numMaxHophints, hopHintFactor, amtMSat,
			totalHintBandwidth,
		)
		if enoughHopHints {
			log.Debugf("Second pass of hop selection has " +
				"sufficient hints")

			return hopHints
		}

		channel := openChannels[i]

		// Skip the channel if we already selected it.
		if _, ok := hopHintChans[channel.FundingOutpoint]; ok {
			continue
		}

		// If the channel can't be a hop hint, then we'll skip it.
		// Otherwise, we'll use the policy information to populate the
		// hop hint.
		remotePolicy, canBeHopHint := chanCanBeHopHint(channel, cfg)
		if !canBeHopHint || remotePolicy == nil {
			continue
		}

		// Lookup and see if there's an alias SCID that exists.
		chanID := lnwire.NewChanIDFromOutPoint(
			&channel.FundingOutpoint,
		)
		alias, _ := cfg.GetAlias(chanID)

		// If this is a channel where the option-scid-alias feature bit
		// was negotiated and the alias is not yet assigned, we cannot
		// issue an invoice. Doing so might expose the confirmed SCID
		// of a private channel.
		if channel.ScidAliasFeature {
			var defaultScid lnwire.ShortChannelID
			if alias == defaultScid {
				continue
			}
		}

		// Include the route hint in our set of options that will be
		// used when creating the invoice.
		addHopHint(&hopHints, channel, remotePolicy, alias)

		// As we've just added a new hop hint, we'll accumulate it's
		// available balance now to update our tally.
		//
		// TODO(roasbeef): have a cut off based on min bandwidth?
		totalHintBandwidth += channel.RemoteBalance
	}

	return hopHints
}
