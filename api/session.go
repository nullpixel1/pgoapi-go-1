package api

import (
	"golang.org/x/net/context"
	"crypto/rand"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/golang/protobuf/jsonpb"
	"github.com/golang/protobuf/proto"

	"errors"

	"github.com/muxgo/pgoapi-go/auth"
	"github.com/muxgo/pgoapi-go/newcrypto"
	protos "github.com/pogodevorg/POGOProtos-go"
)

const defaultURL = "https://pgorelease.nianticlabs.com/plfe/rpc"
const downloadSettingsHash = "05daf51635c82611d1aac95c0b051d3ec088a930"

// Session is used to communicate with the Pokémon Go API
type Session struct {
	feed     Feed
	signer   *newcrypto.PogoSignature
	location *Location
	rpc      *RPC
	RPCID    uint64
	url      string
	debug    bool
	debugger *jsonpb.Marshaler

	hasTicket bool
	ticket    *protos.AuthTicket
	started   time.Time
	provider  auth.Provider
	hash      []byte
}

func generateRequests() []*protos.Request {
	return make([]*protos.Request, 0)
}

func getTimestamp(t time.Time) uint64 {
	return uint64(t.UnixNano() / int64(time.Millisecond))
}

// NewSession constructs a Pokémon Go RPC API client
func NewSession(signer *newcrypto.PogoSignature, provider auth.Provider, location *Location, feed Feed, debug bool) *Session {
	return &Session{
		location:  location,
		rpc:       NewRPC(),
		signer:    signer,
		provider:  provider,
		debug:     debug,
		debugger:  &jsonpb.Marshaler{Indent: "\t"},
		feed:      feed,
		started:   time.Now(),
		hasTicket: false,
		hash:      make([]byte, 32),
	}
}

// IsExpired checks the expiration timestamp of the sessions AuthTicket
// if the session has a ticket and it is still valid, the return value is false
// if there is no ticket, or the ticket is expired, the return value is true
func (s *Session) IsExpired() bool {
	if !s.hasTicket || s.ticket == nil {
		return true
	}
	return s.ticket.ExpireTimestampMs < getTimestamp(time.Now())
}

// SetTimeout sets the client timeout for the RPC API
func (s *Session) SetTimeout(d time.Duration) {
	s.rpc.http.Timeout = d
}

func (s *Session) setTicket(ticket *protos.AuthTicket) {
	s.hasTicket = true
	s.ticket = ticket
}

func (s *Session) setURL(urlToken string) {
	s.url = fmt.Sprintf("https://%s/rpc", urlToken)
}

func (s *Session) getURL() string {
	var url string
	if s.url != "" {
		url = s.url
	} else {
		url = defaultURL
	}
	return url
}

func (s *Session) debugProtoMessage(label string, pb proto.Message) {
	if s.debug {
		str, _ := s.debugger.MarshalToString(pb)
		log.Println(fmt.Sprintf("%s: %s", label, str))
	}
}

// Call queries the Pokémon Go API through RPC protobuf
func (s *Session) Call(ctx context.Context, requests []*protos.Request, proxyId int64) (*protos.ResponseEnvelope, error) {

	requestEnvelope := &protos.RequestEnvelope{
		RequestId:  uint64(8145806132888207460),
		StatusCode: int32(2),

		MsSinceLastLocationfix: int64(989),

		Longitude: s.location.Lon,
		Latitude:  s.location.Lat,

		Accuracy: s.location.Accuracy,

		Requests: requests,
	}

	if s.hasTicket {
		requestEnvelope.AuthTicket = s.ticket
	} else {
		requestEnvelope.AuthInfo = &protos.RequestEnvelope_AuthInfo{
			Provider: s.provider.GetProviderString(),
			Token: &protos.RequestEnvelope_AuthInfo_JWT{
				Contents: s.provider.GetAccessToken(),
				Unknown2: int32(59),
			},
		}
	}

	if s.hasTicket {
		t := getTimestamp(time.Now())

		requestHash := make([]uint64, len(requests))
		ticket, err := proto.Marshal(s.ticket)
		if err != nil {
			return nil, err
		}

		for idx, request := range requests {
			req, err := proto.Marshal(request)
			if err != nil {
				return nil, err
			}
			hash := s.signer.HashRequest(ticket, req)
			requestHash[idx] = hash
		}

		locationHash1 := s.signer.HashLocation1(ticket, s.location.Lat, s.location.Lon, s.location.Alt)
		locationHash2 := s.signer.HashLocation2(s.location.Lat, s.location.Lon, s.location.Alt)

		signature := &protos.Signature{
			RequestHash:   requestHash,
			LocationHash1: locationHash1,
			LocationHash2: locationHash2,
			ActivityStatus: &protos.Signature_ActivityStatus{
				Stationary: true,
			},
			DeviceInfo: &protos.Signature_DeviceInfo{
				DeviceId:             "<device_id>",
				DeviceBrand:          "Apple",
				DeviceModel:          "iPhone",
				DeviceModelBoot:      "Iphone7,2",
				HardwareManufacturer: "Apple",
				HardwareModel:        "N66AP",
				FirmwareBrand:        "iPhone OS",
				FirmwareType:         "9.3.3",
			},
			SessionHash:         s.hash,
			Timestamp:           t,
			TimestampSinceStart: (t - getTimestamp(s.started)),
			Unknown25:           s.signer.Hash25(),
		}

		signatureProto, err := proto.Marshal(signature)
		if err != nil {
			return nil, ErrFormatting
		}

		encryptedSignature := newcrypto.Encrypt(signatureProto, uint32(signature.TimestampSinceStart))

		requestMessage, err := proto.Marshal(&protos.SendEncryptedSignatureRequest{
			EncryptedSignature: encryptedSignature,
		})
		if err != nil {
			return nil, ErrFormatting
		}

		requestEnvelope.PlatformRequests = []*protos.RequestEnvelope_PlatformRequest{
			{
				Type:           protos.PlatformRequestType_SEND_ENCRYPTED_SIGNATURE,
				RequestMessage: requestMessage,
			},
		}

		s.debugProtoMessage("request signature", signature)
	}

	s.debugProtoMessage("request envelope", requestEnvelope)

	responseEnvelope, err := s.rpc.Request(ctx, s.getURL(), requestEnvelope, proxyId)

	s.debugProtoMessage("response envelope", responseEnvelope)

	return responseEnvelope, err
}

// MoveTo sets your current location
func (s *Session) MoveTo(location *Location) {
	s.location = location
}

// Init initializes the client by performing full authentication
func (s *Session) Init(ctx context.Context, proxyId int64) error {
	_, err := s.provider.Login(ctx)
	if err != nil {
		return err
	}

	_, err = rand.Read(s.hash)
	if err != nil {
		return ErrFormatting
	}

	settingsMessage, _ := proto.Marshal(&protos.DownloadSettingsMessage{
		Hash: downloadSettingsHash,
	})

	requests := []*protos.Request{
		{RequestType: protos.RequestType_GET_PLAYER},
		{RequestType: protos.RequestType_GET_HATCHED_EGGS},
		{RequestType: protos.RequestType_GET_INVENTORY},
		{RequestType: protos.RequestType_CHECK_AWARDED_BADGES},
		{protos.RequestType_DOWNLOAD_SETTINGS, settingsMessage},
		// {RequestType: protos.RequestType_CHECK_CHALLENGE},
	}

	response, err := s.Call(ctx, requests, proxyId)
	if err != nil {
		return err
	}

	url := response.ApiUrl
	if url == "" {
		return ErrNoURL
	}
	s.setURL(url)

	ticket := response.GetAuthTicket()

	s.setTicket(ticket)

	return nil
}

// Announce publishes the player's presence and returns the map environment
func (s *Session) Announce(ctx context.Context, proxyId int64) (mapObjects *protos.GetMapObjectsResponse, err error) {
	cellIDs := s.location.GetCellIDs()
	lastTimestamp := time.Now().Unix() * 1000

	settingsMessage, _ := proto.Marshal(&protos.DownloadSettingsMessage{
		Hash: downloadSettingsHash,
	})

	getMapObjs := &protos.GetMapObjectsMessage{
		// Traversed route since last supposed last heartbeat
		CellId: cellIDs,

		// Timestamps in milliseconds corresponding to each route cell id
		SinceTimestampMs: make([]int64, len(cellIDs)),

		// Current longitide and latitude
		Longitude: s.location.Lon,
		Latitude:  s.location.Lat,
	}

	// Request the map objects based on my current location and route cell ids
	getMapObjectsMessage, _ := proto.Marshal(getMapObjs)

	s.debugProtoMessage("mapObjects", getMapObjs)

	// Request the inventory with a message containing the current time
	getInventoryMessage, _ := proto.Marshal(&protos.GetInventoryMessage{
		LastTimestampMs: lastTimestamp,
	})
	requests := []*protos.Request{
		{RequestType: protos.RequestType_GET_PLAYER},
		{RequestType: protos.RequestType_GET_HATCHED_EGGS},
		{protos.RequestType_GET_INVENTORY, getInventoryMessage},
		{RequestType: protos.RequestType_CHECK_AWARDED_BADGES},
		{protos.RequestType_DOWNLOAD_SETTINGS, settingsMessage},
		{protos.RequestType_GET_MAP_OBJECTS, getMapObjectsMessage},
		{RequestType: protos.RequestType_CHECK_CHALLENGE},
	}

	response, err := s.Call(ctx, requests, proxyId)
	if err != nil {
		if err == ErrProxyDead {
			return mapObjects, err
		}
		return mapObjects, ErrRequest
	}

	mapObjects = &protos.GetMapObjectsResponse{}
	if len(response.Returns) < 5 {
		return nil, errors.New("Empty response")
	}
	err = proto.Unmarshal(response.Returns[5], mapObjects)
	if err != nil {
		return nil, &ErrResponse{err}
	}
	s.feed.Push(mapObjects)
	s.debugProtoMessage("response return[5]", mapObjects)

	challenge := protos.CheckChallengeResponse{}
	err = proto.Unmarshal(response.Returns[0], &challenge)
	if challenge.ShowChallenge {
		if strings.Contains(challenge.ChallengeUrl, "new RPC url") {
			s.setURL(response.ApiUrl)
		}
		return mapObjects, nil
	}

	return mapObjects, GetErrorFromStatus(response.StatusCode)
}

func (s *Session) CheckChallenge(ctx context.Context) (*protos.CheckChallengeResponse, error) {
	requests := []*protos.Request{
		{RequestType: protos.RequestType_CHECK_CHALLENGE},
	}
	response, err := s.Call(ctx, requests, -1)
	if err != nil {
		return nil, err
	}

	if len(response.Returns) < 1 {
		return nil, errors.New("Empty response")
	}

	challenge := &protos.CheckChallengeResponse{}
	err = proto.Unmarshal(response.Returns[0], challenge)
	if err != nil {
		return nil, &ErrResponse{err}
	}
	s.feed.Push(challenge)
	s.debugProtoMessage("response return[0]", challenge)

	return challenge, GetErrorFromStatus(response.StatusCode)
}

func (s *Session) SolveCaptcha(ctx context.Context, solution string) (*protos.VerifyChallengeResponse, error) {
	requestMessage, err := proto.Marshal(&protos.VerifyChallengeMessage{
		Token: solution,
	})
	if err != nil {
		return nil, ErrFormatting
	}

	requests := []*protos.Request{{RequestType: protos.RequestType_VERIFY_CHALLENGE, RequestMessage: requestMessage}}
	response, err := s.Call(ctx, requests, -1)
	if err != nil {
		return nil, err
	}

	if len(response.Returns) < 1 {
		return nil, errors.New("Empty response")
	}

	challenge := &protos.VerifyChallengeResponse{}
	err = proto.Unmarshal(response.Returns[0], challenge)
	if err != nil {
		return nil, &ErrResponse{err}
	}
	s.feed.Push(challenge)
	s.debugProtoMessage("response return[0]", challenge)

	return challenge, GetErrorFromStatus(response.StatusCode)
}

func (s *Session) GetPlayer(ctx context.Context, proxyId int64) (*protos.GetPlayerResponse, error) {
	requests := []*protos.Request{{RequestType: protos.RequestType_GET_PLAYER}}
	response, err := s.Call(ctx, requests, proxyId)
	if err != nil {
		return nil, err
	}

	player := &protos.GetPlayerResponse{}
	err = proto.Unmarshal(response.Returns[0], player)
	if err != nil {
		return nil, &ErrResponse{err}
	}
	s.feed.Push(player)
	s.debugProtoMessage("response return[0]", player)

	return player, GetErrorFromStatus(response.StatusCode)
}

func (s *Session) Encounter(ctx context.Context, encounterID uint64, spawnID string, loc *Location, proxyId int64) (*protos.EncounterResponse, error) {
	requestMessage, err := proto.Marshal(&protos.EncounterMessage{
		EncounterId:     encounterID,
		SpawnPointId:    spawnID,
		PlayerLatitude:  loc.Lat,
		PlayerLongitude: loc.Lon,
	})
	if err != nil {
		return nil, ErrFormatting
	}

	requests := []*protos.Request{{RequestType: protos.RequestType_ENCOUNTER, RequestMessage: requestMessage}}
	response, err := s.Call(ctx, requests, proxyId)
	if err != nil {
		return nil, err
	}

	if len(response.Returns) < 1 {
		return nil, errors.New("Empty response")
	}

	encounter := &protos.EncounterResponse{}
	err = proto.Unmarshal(response.Returns[0], encounter)
	if err != nil {
		return nil, &ErrResponse{err}
	}
	s.feed.Push(encounter)
	s.debugProtoMessage("response return[0]", encounter)

	return encounter, GetErrorFromStatus(response.StatusCode)
}

// GetPlayerMap returns the surrounding map cells
func (s *Session) GetPlayerMap(ctx context.Context, proxyId int64) (*protos.GetMapObjectsResponse, error) {
	return s.Announce(ctx, proxyId)
}

// GetInventory returns the player items
func (s *Session) GetInventory(ctx context.Context, proxyId int64) (*protos.GetInventoryResponse, error) {
	requests := []*protos.Request{{RequestType: protos.RequestType_GET_INVENTORY}}
	response, err := s.Call(ctx, requests, proxyId)
	if err != nil {
		return nil, err
	}
	inventory := &protos.GetInventoryResponse{}
	err = proto.Unmarshal(response.Returns[0], inventory)
	if err != nil {
		return nil, &ErrResponse{err}
	}
	s.feed.Push(inventory)
	s.debugProtoMessage("response return[0]", inventory)

	return inventory, GetErrorFromStatus(response.StatusCode)
}
