//go:build !js
// +build !js

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"

	"github.com/dgraph-io/badger/v3"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media/samplebuilder"
	"github.com/spf13/viper"
	"go.uber.org/zap"
)

// /This server allow duplicate name for different device
// /unless device under same name connect to the server at the same time

var (
	videoTrack map[string]*webrtc.TrackLocalStaticSample
	upgrader   = websocket.Upgrader{} // use default options
	logger     = zap.SugaredLogger{}
)

type CameraConnection struct {
	Name string `JSON:"name"`
	Uuid string `JSON:"uuid"`
}

type ConnectionReply struct {
	Status bool   `JSON:"status"` //true if connection ok
	Error  string `JSON:"error"`  //description of error if failed
	Uuid   string `JSON: "uuid"`  //uuid for matching device
}

func webSocketListener(w http.ResponseWriter, r *http.Request, db *badger.DB) {
	logger.Info("websocket connection receive from :", r.RemoteAddr)
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.Error("upgrade:", err)
		return
	}
	defer c.Close()
	for {
		deviceData := CameraConnection{}
		err := c.ReadJSON(&deviceData)
		if err != nil {
			logger.Error("read:", err)
			c.WriteJSON(ConnectionReply{
				Status: false,
			})
			continue
		}

		deviceUUid := deviceData.Uuid
		deviceName := deviceData.Name

		txn := db.NewTransaction(true)

		if deviceName == "" && deviceUUid != "" { //if the device connected before
			storedName, err := txn.Get([]byte(deviceUUid))
			if err != nil {
				c.WriteJSON(ConnectionReply{
					Status: false,
					Error:  "DB read error",
				})
				continue
			}
			deviceName = storedName.String()
			logger.Info("Device Connected: ", deviceName)

		}

		//search for duplicate on current session
		duplicate := false
		for k, _ := range videoTrack {
			if k == deviceName {
				duplicate = true
				break
			}
		}
		if duplicate {
			logger.Error("Device name conflicted :", deviceName)
			err = c.WriteJSON(ConnectionReply{ //device with same name exist on the server
				Status: false,
				Error:  "Another device with same name already connected at the moment",
			})
			continue
		}

		if deviceUUid == "" { //if it is first time the device connect or uuid not found and no duplicate of name
			deviceUUid = uuid.NewString()
			err := txn.Set([]byte(deviceUUid), []byte(deviceName))
			if err != nil {
				logger.Error("Error write to DB: ", err)
				err = c.WriteJSON(ConnectionReply{ //device with same name exist on the server
					Status: false,
					Error:  "Error write to DB",
				})
				continue
			}
		}

		err = c.WriteJSON(ConnectionReply{ //success register device
			Status: true,
			Uuid:   deviceUUid,
		})
		if err != nil {
			log.Println("write:", err)
			break
		}
	}
}

func main() {
	//init logger
	alogger, _ := zap.NewProduction()
	logger = *alogger.Sugar()
	defer logger.Sync() // flushes buffer, if any

	///define configuration name and load from it
	viper.SetConfigName("config")
	viper.SetConfigType("yaml")

	///define default value for config file
	viper.SetDefault("HttpPort", "8543")

	///read config from file
	err := viper.ReadInConfig() // Find and read the config file
	if err != nil {             // Handle errors reading the config file
		log.Panic(fmt.Errorf("fatal error config file: %w", err))
		logger.Warn("create new confg file: ./config.yaml")
		viper.WriteConfig()
	}

	//open KV database
	db, err := badger.Open(badger.DefaultOptions("./badger"))

	httpServePort := viper.GetString("HttpPort")

	//init variable
	videoTrack = map[string]*webrtc.TrackLocalStaticSample{}

	//first open a http server
	http.Handle("/", http.FileServer(http.Dir("./static")))

	// Open a UDP Listener for RTP Packets on port 5004
	/*
			listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("0.0.0.0"), Port: 5004})
			if err != nil {
				panic(err)
			}

		defer func() {
			if err = listener.Close(); err != nil {
				panic(err)
			}
		}()
	*/

	http.HandleFunc("/wrtc", func(rw http.ResponseWriter, req *http.Request) {
		// Wait for the offer to be pasted
		p := req.URL.Query().Get("device")
		if p == "" {
			rw.WriteHeader(http.StatusTeapot)
			return
		}
		if _, ok := videoTrack[p]; !ok {
			rw.WriteHeader(http.StatusTeapot)
			return
		}
		c, err := io.ReadAll(req.Body)
		offer := webrtc.SessionDescription{}
		err = json.Unmarshal(c, &offer)
		if err != nil {
			panic(err)
		}

		peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{
			ICEServers: []webrtc.ICEServer{
				{
					URLs: stunList,
				},
			},
		})
		if err != nil {
			panic(err)
		}
		/*
			rtpSender, err := peerConnection.AddTrack(videoTrack[p])
			if err != nil {
				panic(err)
			}
		*/

		// Read incoming RTCP packets
		// Before these packets are returned they are processed by interceptors. For things
		// like NACK this needs to be called.
		go func() {
			rtcpBuf := make([]byte, 4800)
			for {
				if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
					return
				}
			}
		}()

		// Set the handler for ICE connection state
		// This will notify you when the peer has connected/disconnected
		peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
			fmt.Printf("Connection State has changed %s \n", connectionState.String())

			if connectionState == webrtc.ICEConnectionStateFailed {
				if closeErr := peerConnection.Close(); closeErr != nil {
					panic(closeErr)
				}
			}
		})

		// Set the remote SessionDescription
		if err = peerConnection.SetRemoteDescription(offer); err != nil {
			panic(err)
		}

		// Create answer
		answer, err := peerConnection.CreateAnswer(nil)
		if err != nil {
			panic(err)
		}

		// Create channel that is blocked until ICE Gathering is complete
		gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

		// Sets the LocalDescription, and starts our UDP listeners
		if err = peerConnection.SetLocalDescription(answer); err != nil {
			panic(err)
		}

		// Block until ICE Gathering is complete, disabling trickle ICE
		// we do this because we only can exchange one signaling message
		// in a production application you should exchange ICE Candidates via OnICECandidate
		<-gatherComplete

		b, err := json.Marshal(peerConnection.LocalDescription())
		if err != nil {
			panic(err)
		}
		rw.Header().Set("Content-Type", "application/json")
		rw.Write(b)
		log.Print("client added")
	})
	go func() {
		logger.Info("HTTP server started on: ", httpServePort)
		http.ListenAndServe(":"+string(httpServePort), nil)
	}()

	/*
		s := &gortsplib.Server{
			Handler: &serverHandler{
				stream: map[*gortsplib.ServerSession]struct {
					stream *gortsplib.ServerStream
					path   string
				}{},
			},
			RTSPAddress: ":8083",
		}
	*/

	// start server and wait until a fatal error
	log.Printf("server is ready")
	vb := map[string]*samplebuilder.SampleBuilder{}
	//ml := &sync.WaitGroup{}
	pc := map[string]chan *rtp.Packet{}
	for {
		inboundRTPPacket := make([]byte, 1600) // UDP MTU
		n, a, err := listener.ReadFromUDP(inboundRTPPacket)

		packet := &rtp.Packet{}
		if _, ok := videoTrack[a.String()]; !ok { //not exist
			webrtc, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "video", "pion")
			if err != nil {
				continue
			}
			vb[a.String()] = samplebuilder.New(10, &codecs.H264Packet{}, 90000)
			videoTrack[a.String()] = webrtc
			pc[a.String()] = make(chan *rtp.Packet, 1200)
			log.Print(a.String())
			go func(addr string) {
				for pkt := range pc[addr] {
					vb[addr].Push(pkt)
					for {
						sample := vb[addr].Pop()
						if sample == nil {
							break
						}

						if writeErr := videoTrack[addr].WriteSample(*sample); writeErr != nil {
							panic(writeErr)
						}
					}
				}
			}(a.String())
		}

		if err = packet.Unmarshal(inboundRTPPacket[:n]); err != nil {
			panic(err)
		}
		pc[a.String()] <- packet
		/*
			if _, err = videoTrack[a.String()].Write(inboundRTPPacket[:n]); err != nil {
				if errors.Is(err, io.ErrClosedPipe) {
					// The peerConnection has been closed.
					return
				}

				panic(err)
			}
		*/
	}
	//panic(s.StartAndWait())

}
