package core

import (
	"context"
	"encoding/json"
	"net/http"

	// "encoding/hex"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	// "io"
	// "net/http"
	"log"
	"os"

	"github.com/aler9/gortsplib"
	"github.com/aler9/gortsplib/pkg/base"
	"github.com/google/uuid"
	"github.com/pion/rtp"

	"github.com/bhaney/rtsp-simple-server/internal/conf"
	"github.com/bhaney/rtsp-simple-server/internal/externalcmd"
	"github.com/bhaney/rtsp-simple-server/internal/logger"

	// AWS SDK v2 imports
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Global variables
var (
	dbSvc                 *dynamodb.Client
	activeSessionCount    int
	countMutex            sync.Mutex
	dynamoDBTableName     string
	recordFargateMetadata map[string]types.AttributeValue
)

func init() {
	// Load the AWS configuration from the environment, credentials file, or IAM role
	cfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithRegion("us-east-1"), // Replace with your desired region
	)
	if err != nil {
		panic("unable to load SDK config, " + err.Error())
	}

	// Initialize DynamoDB client with the configuration
	dbSvc = dynamodb.NewFromConfig(cfg)

	dynamoDBTableName = os.Getenv("DYNAMODB_TABLE_NAME")
	if dynamoDBTableName == "" {
		log.Fatal("DYNAMODB_TABLE_NAME environment variable is not set")
		dynamoDBTableName = "sam-rtsp-virtual-streams"

	}

}

// func getInstanceID() string {
// 	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
// 	defer cancel()

// 	client := imds.New(imds.Options{})

// 	// Get instance ID from IMDS
// 	instanceID, err := client.GetInstanceIdentityDocument(ctx, &imds.GetInstanceIdentityDocumentInput{})

// 	if err != nil {
// 		return "local-instance" // fallback for local testing
// 	}

// 	return instanceID.InstanceID
// }

const (
	pauseAfterAuthError = 2 * time.Second
)

type rtspSessionPathManager interface {
	publisherAdd(req pathPublisherAddReq) pathPublisherAnnounceRes
	readerAdd(req pathReaderAddReq) pathReaderSetupPlayRes
}

type rtspSessionParent interface {
	log(logger.Level, string, ...interface{})
}

type rtspSession struct {
	isTLS           bool
	protocols       map[conf.Protocol]struct{}
	session         *gortsplib.ServerSession
	author          *gortsplib.ServerConn
	externalCmdPool *externalcmd.Pool
	pathManager     rtspSessionPathManager
	parent          rtspSessionParent

	uuid       uuid.UUID
	created    time.Time
	path       *path
	stream     *stream
	state      gortsplib.ServerSessionState
	stateMutex sync.Mutex
	onReadCmd  *externalcmd.Cmd // read
}

func newRTSPSession(
	isTLS bool,
	protocols map[conf.Protocol]struct{},
	session *gortsplib.ServerSession,
	sc *gortsplib.ServerConn,
	externalCmdPool *externalcmd.Pool,
	pathManager rtspSessionPathManager,
	parent rtspSessionParent,
) *rtspSession {
	s := &rtspSession{
		isTLS:           isTLS,
		protocols:       protocols,
		session:         session,
		author:          sc,
		externalCmdPool: externalCmdPool,
		pathManager:     pathManager,
		parent:          parent,
		uuid:            uuid.New(),
		created:         time.Now(),
	}
	s.log(logger.Debug, "rtsp_session.go> newRTSPSession: Begin")

	// s.log(logger.Info, "created by %v", s.author.NetConn().RemoteAddr())
	s.log(logger.Debug, "rtsp_session.go> newRTSPSession: End-99")

	return s
}

// Close closes a Session.
func (s *rtspSession) close() {
	s.log(logger.Debug, "close: Begin")
	s.session.Close()
	s.log(logger.Debug, "close: End-99")

}

// isRTSPSession implements pathRTSPSession.
func (s *rtspSession) isRTSPSession() {}

func (s *rtspSession) safeState() gortsplib.ServerSessionState {
	s.log(logger.Debug, "safeState: Begin")
	s.stateMutex.Lock()
	defer s.stateMutex.Unlock()
	s.log(logger.Debug, "safeState: End-99")
	return s.state
}

func (s *rtspSession) remoteAddr() net.Addr {
	return s.author.NetConn().RemoteAddr()
}

func (s *rtspSession) log(level logger.Level, format string, args ...interface{}) {
	id := s.uuid.String()
	s.parent.log(level, "[session %s] "+format, append([]interface{}{id}, args...)...)
}

// onClose is called by rtspServer.
func (s *rtspSession) onClose(err error) {
	s.log(logger.Debug, "onClose: Begin")
	if s.session.State() == gortsplib.ServerSessionStatePlay {
		if s.onReadCmd != nil {
			s.onReadCmd.Close()
			s.onReadCmd = nil
			s.log(logger.Info, "runOnRead command stopped")
		}
	}

	switch s.session.State() {
	case gortsplib.ServerSessionStatePrePlay, gortsplib.ServerSessionStatePlay:
		s.path.readerRemove(pathReaderRemoveReq{author: s})

	case gortsplib.ServerSessionStatePreRecord, gortsplib.ServerSessionStateRecord:

		s.path.publisherRemove(pathPublisherRemoveReq{author: s})
		countMutex.Lock()
		activeSessionCount--
		formattedSessionCount := fmt.Sprintf("%06d", activeSessionCount) // Pads to 6 digits with leading zeros
		countMutex.Unlock()
		timestamp := time.Now().UTC().Format(time.RFC3339)

		// fmt.Printf(timestamp, " | %s | STOPPED | %s | %s\n", formattedSessionCount, s.uuid, s.path.Name())
		s.log(logger.Info, "| %s | STOPPED | %s", formattedSessionCount, s.path.Name())

		// Only log to DynamoDB and print stop message for publishers

		input := &dynamodb.UpdateItemInput{
			TableName: aws.String(dynamoDBTableName),
			Key: map[string]types.AttributeValue{
				"stream_id": &types.AttributeValueMemberS{
					Value: s.path.Name(),
				},
			},
			UpdateExpression:    aws.String("SET time_disconnected = :time_disconnected, is_active = :is_active"),
			ConditionExpression: aws.String("is_active = :is_active_condition"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":time_disconnected": &types.AttributeValueMemberS{
					Value: timestamp,
				},
				":is_active": &types.AttributeValueMemberBOOL{
					Value: false,
				},
				":is_active_condition": &types.AttributeValueMemberBOOL{
					Value: true,
				},
			},
		}
		go func() {
			_, err := dbSvc.UpdateItem(context.TODO(), input) // Passing context as required
			if err != nil {
				s.log(logger.Error, "failed to log stream stop to DynamoDB: %v", err)
			}
		}()
	}
	s.log(logger.Debug, "onClose: End-99")

	s.path = nil
	s.stream = nil
}

// onAnnounce is called by rtspServer.
func (s *rtspSession) onAnnounce(c *rtspConn, ctx *gortsplib.ServerHandlerOnAnnounceCtx) (*base.Response, error) {
	s.log(logger.Debug, "onAnnounce: Begin")
	res := s.pathManager.publisherAdd(pathPublisherAddReq{
		author:   s,
		pathName: ctx.Path,
		authenticate: func(
			pathIPs []fmt.Stringer,
			pathUser conf.Credential,
			pathPass conf.Credential,
		) error {
			return c.authenticate(ctx.Path, pathIPs, pathUser, pathPass, true, ctx.Request, ctx.Query)
		},
	})

	if res.err != nil {
		switch terr := res.err.(type) {
		case pathErrAuthNotCritical:
			s.log(logger.Debug, "non-critical authentication error: %s", terr.message)
			return terr.response, nil

		case pathErrAuthCritical:
			// wait some seconds to stop brute force attacks
			<-time.After(pauseAfterAuthError)

			return terr.response, errors.New(terr.message)

		default:
			return &base.Response{
				StatusCode: base.StatusBadRequest,
			}, res.err
		}
	}

	s.path = res.path

	s.stateMutex.Lock()
	s.state = gortsplib.ServerSessionStatePreRecord
	s.stateMutex.Unlock()
	s.log(logger.Debug, "onAnnounce: End-99")

	return &base.Response{
		StatusCode: base.StatusOK,
	}, nil
}

// onSetup is called by rtspServer.
func (s *rtspSession) onSetup(c *rtspConn, ctx *gortsplib.ServerHandlerOnSetupCtx,
) (*base.Response, *gortsplib.ServerStream, error) {
	s.log(logger.Debug, "onSetup: Begin")
	// in case the client is setupping a stream with UDP or UDP-multicast, and these
	// transport protocols are disabled, gortsplib already blocks the request.
	// we have only to handle the case in which the transport protocol is TCP
	// and it is disabled.
	if ctx.Transport == gortsplib.TransportTCP {
		if _, ok := s.protocols[conf.Protocol(gortsplib.TransportTCP)]; !ok {
			s.log(logger.Debug, "onSetup: End-1")
			return &base.Response{
				StatusCode: base.StatusUnsupportedTransport,
			}, nil, nil
		}
	}

	switch s.session.State() {
	case gortsplib.ServerSessionStateInitial, gortsplib.ServerSessionStatePrePlay: // play
		res := s.pathManager.readerAdd(pathReaderAddReq{
			author:   s,
			pathName: ctx.Path,
			authenticate: func(
				pathIPs []fmt.Stringer,
				pathUser conf.Credential,
				pathPass conf.Credential,
			) error {
				s.log(logger.Debug, "onSetup: End-1")
				return c.authenticate(ctx.Path, pathIPs, pathUser, pathPass, false, ctx.Request, ctx.Query)
			},
		})

		if res.err != nil {
			switch terr := res.err.(type) {
			case pathErrAuthNotCritical:
				s.log(logger.Debug, "non-critical authentication error: %s", terr.message)
				s.log(logger.Debug, "onSetup: End-2")
				return terr.response, nil, nil

			case pathErrAuthCritical:
				// wait some seconds to stop brute force attacks
				<-time.After(pauseAfterAuthError)

				s.log(logger.Debug, "onSetup: End-3")
				return terr.response, nil, errors.New(terr.message)

			case pathErrNoOnePublishing:
				s.log(logger.Debug, "onSetup: End-4")
				return &base.Response{
					StatusCode: base.StatusNotFound,
				}, nil, res.err

			default:
				s.log(logger.Debug, "onSetup: End-5")
				return &base.Response{
					StatusCode: base.StatusBadRequest,
				}, nil, res.err
			}
		}

		s.path = res.path
		s.stream = res.stream

		if ctx.TrackID >= len(res.stream.tracks()) {
			s.log(logger.Debug, "onSetup: End-6")
			return &base.Response{
				StatusCode: base.StatusBadRequest,
			}, nil, fmt.Errorf("track %d does not exist", ctx.TrackID)
		}

		s.stateMutex.Lock()
		s.state = gortsplib.ServerSessionStatePrePlay
		s.stateMutex.Unlock()

		s.log(logger.Debug, "onSetup: End-7")
		return &base.Response{
			StatusCode: base.StatusOK,
		}, res.stream.rtspStream, nil

	default: // record
		s.log(logger.Debug, "onSetup: End-99")
		return &base.Response{
			StatusCode: base.StatusOK,
		}, nil, nil
	}
}

// onPlay is called by rtspServer.
func (s *rtspSession) onPlay(ctx *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	s.log(logger.Debug, "onPlay: Begin")
	h := make(base.Header)

	if s.session.State() == gortsplib.ServerSessionStatePrePlay {
		s.path.readerStart(pathReaderStartReq{author: s})

		tracks := make(gortsplib.Tracks, len(s.session.SetuppedTracks()))
		n := 0
		for id := range s.session.SetuppedTracks() {
			tracks[n] = s.stream.tracks()[id]
			n++
		}

		// Only log basic info for readers, no special formatting
		s.log(logger.Info, "is reading from path '%s', with %s, %s",
			s.path.Name(),
			s.session.SetuppedTransport(),
			sourceTrackInfo(tracks))

		if s.path.Conf().RunOnRead != "" {
			s.log(logger.Info, "runOnRead command started")
			s.onReadCmd = externalcmd.NewCmd(
				s.externalCmdPool,
				s.path.Conf().RunOnRead,
				s.path.Conf().RunOnReadRestart,
				s.path.externalCmdEnv(),
				func(co int) {
					s.log(logger.Info, "runOnRead command exited with code %d", co)
				})
		}

		s.stateMutex.Lock()
		s.state = gortsplib.ServerSessionStatePlay
		s.stateMutex.Unlock()
	}
	s.log(logger.Debug, "onPlay: End-99")

	return &base.Response{
		StatusCode: base.StatusOK,
		Header:     h,
	}, nil
}

func (s *rtspSession) onRecord(ctx *gortsplib.ServerHandlerOnRecordCtx) (*base.Response, error) {
	s.log(logger.Debug, "rtsp_session.go> onRecord: Begin")
	res := s.path.publisherStart(pathPublisherStartReq{
		author:             s,
		tracks:             s.session.AnnouncedTracks(),
		generateRTPPackets: false,
	})
	if res.err != nil {
		s.log(logger.Debug, "rtsp_session.go> onRecord: End-1")
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, res.err
	}
	countMutex.Lock()

	activeSessionCount++
	formattedSessionCount := fmt.Sprintf("%06d", activeSessionCount) // Pads to 6 digits with leading zeros
	countMutex.Unlock()

	s.log(logger.Info, "| %s | STARTED | %s", formattedSessionCount, s.path.Name())

	// Log publisher start
	// fmt.Println("[",s.path.Name(),"]",":", s.uuid, ">>> Started")

	// Log to DynamoDB for publishers
	if server_environment == "Fargate" {
		update_Fargate_Stream_info_DynamoDB(s.path.Name(), s.uuid.String(), s.author.NetConn().RemoteAddr().String())
	} else {
		update_EC2_Stream_info_DynamoDB(s.path.Name(), s.uuid.String(), s.author.NetConn().RemoteAddr().String())

	}

	s.stream = res.stream

	s.stateMutex.Lock()
	s.state = gortsplib.ServerSessionStateRecord
	s.stateMutex.Unlock()

	s.log(logger.Debug, "rtsp_session.go> onRecord: End-99")

	return &base.Response{
		StatusCode: base.StatusOK,
	}, nil
}

// onPause is called by rtspServer.
func (s *rtspSession) onPause(ctx *gortsplib.ServerHandlerOnPauseCtx) (*base.Response, error) {
	s.log(logger.Debug, "onPause: Begin")
	switch s.session.State() {
	case gortsplib.ServerSessionStatePlay:
		if s.onReadCmd != nil {
			s.log(logger.Info, "runOnRead command stopped")
			s.onReadCmd.Close()
		}

		s.path.readerStop(pathReaderStopReq{author: s})

		s.stateMutex.Lock()
		s.state = gortsplib.ServerSessionStatePrePlay
		s.stateMutex.Unlock()

	case gortsplib.ServerSessionStateRecord:
		s.path.publisherStop(pathPublisherStopReq{author: s})

		s.stateMutex.Lock()
		s.state = gortsplib.ServerSessionStatePreRecord
		s.stateMutex.Unlock()
	}

	s.log(logger.Debug, "onPause: End-99")

	return &base.Response{
		StatusCode: base.StatusOK,
	}, nil
}

// onReaderData implements reader.
func (s *rtspSession) onReaderData(data data) {
	// packets are routed to the session by gortsplib.ServerStream.
}

// apiReaderDescribe implements reader.
func (s *rtspSession) apiReaderDescribe() interface{} {
	s.log(logger.Debug, "apiReaderDescribe: Begin")
	var typ string
	if s.isTLS {
		typ = "rtspsSession"
	} else {
		typ = "rtspSession"
	}

	s.log(logger.Debug, "apiReaderDescribe: End-99")
	return struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}{typ, s.uuid.String()}
}

// apiSourceDescribe implements source.
func (s *rtspSession) apiSourceDescribe() interface{} {
	s.log(logger.Debug, "apiSourceDescribe: Begin")
	var typ string
	if s.isTLS {
		typ = "rtspsSession"
	} else {
		typ = "rtspSession"
	}
	s.log(logger.Debug, "apiSourceDescribe: End-99")

	return struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}{typ, s.uuid.String()}
}

// onPacketRTP is called by rtspServer.
func (s *rtspSession) onPacketRTP(ctx *gortsplib.ServerHandlerOnPacketRTPCtx) {
	//s.log(logger.Debug, "rtsp_session.go> onPacketRTP: Begin")
	var err error

	switch s.session.AnnouncedTracks()[ctx.TrackID].(type) {
	case *gortsplib.TrackH264:
		err = s.stream.writeData(&dataH264{
			trackID:    ctx.TrackID,
			rtpPackets: []*rtp.Packet{ctx.Packet},
			ntp:        time.Now(),
		})

	case *gortsplib.TrackMPEG4Audio:
		err = s.stream.writeData(&dataMPEG4Audio{
			trackID:    ctx.TrackID,
			rtpPackets: []*rtp.Packet{ctx.Packet},
			ntp:        time.Now(),
		})

	default:
		err = s.stream.writeData(&dataGeneric{
			trackID:    ctx.TrackID,
			rtpPackets: []*rtp.Packet{ctx.Packet},
			ntp:        time.Now(),
		})
	}

	if err != nil {
		s.log(logger.Warn, "%v", err)
	}
	//s.log(logger.Debug, "rtsp_session.go> onPacketRTP: End-99")
}

// onDecodeError is called by rtspServer.
func (s *rtspSession) onDecodeError(ctx *gortsplib.ServerHandlerOnDecodeErrorCtx) {
	s.log(logger.Warn, "%v", ctx.Error)
}

func update_EC2_Stream_info_DynamoDB(stream_id string, session_id string, streamer_ip_address string) {
	timestamp := time.Now().UTC().Format(time.RFC3339)
	input := &dynamodb.PutItemInput{
		TableName: aws.String(dynamoDBTableName),
		Item: map[string]types.AttributeValue{
			"stream_id": &types.AttributeValueMemberS{
				Value: stream_id,
			},
			"is_active": &types.AttributeValueMemberBOOL{
				Value: true,
			},
			"rtsp_server_id": &types.AttributeValueMemberS{
				Value: server_instance_id,
			},
			"rtsp_server_ip": &types.AttributeValueMemberS{
				Value: server_public_ip,
			},
			"session_id": &types.AttributeValueMemberS{
				Value: session_id,
			},
			"streamer_ip_address": &types.AttributeValueMemberS{
				Value: streamer_ip_address,
			},
			"time_connected": &types.AttributeValueMemberS{
				Value: timestamp,
			},
		},
	}

	go func() {
		_, err := dbSvc.PutItem(context.TODO(), input) // Passing context as required
		if err != nil {
			log.Printf("failed to log stream start to DynamoDB: %v", err)
		}
	}()

}

func update_Fargate_Stream_info_DynamoDB(stream_id string, session_id string, streamer_ip_address string) {
	recordFargateMetadata = getFargateMetadataMap()
	timestamp := time.Now().UTC().Format(time.RFC3339)
	input := &dynamodb.PutItemInput{
		TableName: aws.String(dynamoDBTableName),
		Item: map[string]types.AttributeValue{
			"stream_id": &types.AttributeValueMemberS{
				Value: stream_id,
			},
			"is_active": &types.AttributeValueMemberBOOL{
				Value: true,
			},
			"rtsp_server_id": &types.AttributeValueMemberS{
				Value: server_instance_id,
			},
			"rtsp_server_ip": &types.AttributeValueMemberS{
				Value: server_public_ip,
			},
			"session_id": &types.AttributeValueMemberS{
				Value: session_id,
			},
			"streamer_ip_address": &types.AttributeValueMemberS{
				Value: streamer_ip_address,
			},
			"time_connected": &types.AttributeValueMemberS{
				Value: timestamp,
			},
			"record_fargate_metadata": &types.AttributeValueMemberM{
				Value: recordFargateMetadata,
			},
		},
	}

	go func() {
		_, err := dbSvc.PutItem(context.TODO(), input) // Passing context as required
		if err != nil {
			log.Printf("failed to log stream start to DynamoDB: %v", err)
		}
	}()

}

func getFargateMetadataMap() map[string]types.AttributeValue {
	metadataUri := os.Getenv("ECS_CONTAINER_METADATA_URI_V4")
	if metadataUri == "" {
		metadataUri = "http://169.254.170.2/v4"
	}

	taskEndpoint := metadataUri + "/task"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", taskEndpoint, nil)
	if err != nil {
		log.Printf("Error creating request for Fargate metadata: %v", err)
		return nil
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("Error retrieving Fargate metadata: %v", err)
		return nil
	}
	defer resp.Body.Close()

	// Parse the metadata JSON response into a generic map
	var metadata map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&metadata); err != nil {
		log.Printf("Error decoding Fargate metadata JSON: %v", err)
		return nil
	}

	// Convert JSON map to DynamoDB map structure
	dynamoMap := convertToDynamoDBMap(metadata)
	return dynamoMap
}

// Helper function to recursively convert JSON map to DynamoDB map
func convertToDynamoDBMap(data map[string]interface{}) map[string]types.AttributeValue {
	dynamoMap := make(map[string]types.AttributeValue)
	for key, value := range data {
		switch v := value.(type) {
		case string:
			dynamoMap[key] = &types.AttributeValueMemberS{Value: v}
		case bool:
			dynamoMap[key] = &types.AttributeValueMemberBOOL{Value: v}
		case float64: // AWS DynamoDB uses string or integer, float handling may vary
			dynamoMap[key] = &types.AttributeValueMemberN{Value: fmt.Sprintf("%v", v)}
		case map[string]interface{}:
			dynamoMap[key] = &types.AttributeValueMemberM{Value: convertToDynamoDBMap(v)}
		case []interface{}:
			dynamoMap[key] = &types.AttributeValueMemberL{Value: convertToDynamoDBList(v)}
		}
	}
	return dynamoMap
}

// Helper function to convert a list to DynamoDB list format
func convertToDynamoDBList(data []interface{}) []types.AttributeValue {
	var dynamoList []types.AttributeValue
	for _, item := range data {
		switch v := item.(type) {
		case string:
			dynamoList = append(dynamoList, &types.AttributeValueMemberS{Value: v})
		case bool:
			dynamoList = append(dynamoList, &types.AttributeValueMemberBOOL{Value: v})
		case float64:
			dynamoList = append(dynamoList, &types.AttributeValueMemberN{Value: fmt.Sprintf("%v", v)})
		case map[string]interface{}:
			dynamoList = append(dynamoList, &types.AttributeValueMemberM{Value: convertToDynamoDBMap(v)})
		}
	}
	return dynamoList
}
