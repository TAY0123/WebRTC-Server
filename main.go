//go:build !js
// +build !js

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"time"

	"github.com/dgraph-io/badger/v3"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
	"github.com/spf13/viper"
	"go.uber.org/zap"
)

// /This server allow duplicate name for different device
// /unless device under same name connect to the server at the same time

var (
	upgrader = websocket.Upgrader{} // use default options
	logger   = zap.SugaredLogger{}
)

type Device struct {
	Websocket *websocket.Conn
	Track     *webrtc.TrackLocalStaticRTP
	Listener  *net.UDPConn
	Peer      int
}

type CameraConnection struct {
	Name string `JSON:"name"`
	Uuid string `JSON:"uuid"`
}

type ConnectionReply struct {
	Status bool   `JSON:"status"` //true if connection ok
	Error  string `JSON:"error"`  //description of error if failed
	Uuid   string `JSON:"uuid"`   //uuid for matching device
}

type DeviceStreamStatus struct {
	Stream bool `JSON:"stream"`
	Port   int  `JSON:"port"`
}

func webSocketListener(w http.ResponseWriter, r *http.Request, db *badger.DB, currentDevices *map[string]Device) {
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
		for k := range *currentDevices {
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

		//device every parm ok
		//add a ticker to check if connection ok
		go func(deviceName string) {
			ticker := time.NewTicker(10 * time.Second)
			for range ticker.C {
				if err := c.WriteMessage(websocket.PingMessage, nil); err != nil {
					//if failed to reach client
					delete(*currentDevices, deviceName)
					c.Close()
					return
				}
			}
		}(deviceName)

		(*currentDevices)[deviceName] = Device{
			Websocket: c,
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

	httpServePort := viper.GetString("HttpPort")

	//open KV database
	db, err := badger.Open(badger.DefaultOptions("./badger"))

	//init variable
	onlineDevice := map[string]Device{}

	//add http route and handler
	http.Handle("/", http.FileServer(http.Dir("./static")))

	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		webSocketListener(w, r, db, &onlineDevice)
	})

	http.HandleFunc("/wrtc", func(rw http.ResponseWriter, req *http.Request) {
		// Wait for the offer to be pasted
		p := req.URL.Query().Get("device")
		if p == "" {
			rw.WriteHeader(http.StatusTeapot)
			return
		}
		if _, ok := onlineDevice[p]; !ok {
			rw.WriteHeader(http.StatusTeapot)
			return
		}
		c, err := io.ReadAll(req.Body)
		offer := webrtc.SessionDescription{}
		err = json.Unmarshal(c, &offer)
		if err != nil {
			logger.Error(err)
		}

		current := onlineDevice[p]
		//open port to wait for client stream if current track not exist
		if current.Track == nil {
			port := 40000 + rand.Intn(20000) //between 40000 - 60000
			err = nil
			for err != nil {
				current.Listener, err = net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("0.0.0.0"), Port: port})
				logger.Error("Failed to open Port :", port)
			}
			logger.Info("opened port for ", p, " on : ", port)

			//call websocket to ready stream
			err = current.Websocket.WriteJSON(DeviceStreamStatus{
				Stream: true,
				Port:   port,
			})

			current.Track, err = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "video", "pion")

			go func(current Device) {
				inboundRTPPacket := make([]byte, 1600) // UDP MTU
				for {
					n, _, err := current.Listener.ReadFrom(inboundRTPPacket)
					if err != nil {
						logger.Error(current.Websocket.RemoteAddr(), fmt.Sprintf("error during read: %s", err))
					}

					if _, err = current.Track.Write(inboundRTPPacket[:n]); err != nil {
						logger.Error(current.Websocket.RemoteAddr(), err)
					}
				}
			}(current)
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
		rtpSender, err := peerConnection.AddTrack(current.Track)
		if err != nil {
			panic(err)
		}

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

	// start server and wait until a fatal error
	log.Printf("server is ready")
	a := make(chan bool) //keep main thread
	<-a
}
