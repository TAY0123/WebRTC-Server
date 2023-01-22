//go:build !js
// +build !js

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/dgraph-io/badger/v3"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/huin/goupnp/dcps/internetgateway2"
	"github.com/pion/webrtc/v3"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"golang.org/x/sync/errgroup"
)

// /This server allow duplicate name for different device
// /unless device under same name connect to the server at the same time

var (
	upgrader = websocket.Upgrader{} // use default options
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

type RouterClient interface {
	AddPortMapping(
		NewRemoteHost string,
		NewExternalPort uint16,
		NewProtocol string,
		NewInternalPort uint16,
		NewInternalClient string,
		NewEnabled bool,
		NewPortMappingDescription string,
		NewLeaseDuration uint32,
	) (err error)

	GetExternalIPAddress() (
		NewExternalIPAddress string,
		err error,
	)
}

func PickRouterClient(ctx context.Context) (RouterClient, error) {
	tasks, _ := errgroup.WithContext(ctx)
	// Request each type of client in parallel, and return what is found.
	var ip1Clients []*internetgateway2.WANIPConnection1
	tasks.Go(func() error {
		var err error
		ip1Clients, _, err = internetgateway2.NewWANIPConnection1Clients()
		return err
	})
	var ip2Clients []*internetgateway2.WANIPConnection2
	tasks.Go(func() error {
		var err error
		ip2Clients, _, err = internetgateway2.NewWANIPConnection2Clients()
		return err
	})
	var ppp1Clients []*internetgateway2.WANPPPConnection1
	tasks.Go(func() error {
		var err error
		ppp1Clients, _, err = internetgateway2.NewWANPPPConnection1Clients()
		return err
	})

	if err := tasks.Wait(); err != nil {
		return nil, err
	}

	// Trivial handling for where we find exactly one device to talk to, you
	// might want to provide more flexible handling than this if multiple
	// devices are found.
	switch {
	case len(ip2Clients) == 1:
		return ip2Clients[0], nil
	case len(ip1Clients) == 1:
		return ip1Clients[0], nil
	case len(ppp1Clients) == 1:
		return ppp1Clients[0], nil
	default:
		return nil, errors.New("multiple or no services found")
	}
}

// LocalIP get the host machine local IP address
func LocalIP() (net.IP, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for _, i := range ifaces {
		addrs, err := i.Addrs()
		if err != nil {
			return nil, err
		}

		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}

			if isPrivateIP(ip) {
				return ip, nil
			}
		}
	}

	return nil, errors.New("no IP")
}

func isPrivateIP(ip net.IP) bool {
	var privateIPBlocks []*net.IPNet
	for _, cidr := range []string{
		// don't check loopback ips
		//"127.0.0.0/8",    // IPv4 loopback
		//"::1/128",        // IPv6 loopback
		//"fe80::/10",      // IPv6 link-local
		"10.0.0.0/8",     // RFC1918
		"172.16.0.0/12",  // RFC1918
		"192.168.0.0/16", // RFC1918
	} {
		_, block, _ := net.ParseCIDR(cidr)
		privateIPBlocks = append(privateIPBlocks, block)
	}

	for _, block := range privateIPBlocks {
		if block.Contains(ip) {
			return true
		}
	}

	return false
}

func webSocketListener(w http.ResponseWriter, r *http.Request, db *badger.DB, currentDevices *map[string]Device) {
	logrus.Info("websocket connection receive from :", r.RemoteAddr)
	c, err := upgrader.Upgrade(w, r, nil)

	if err != nil {
		logrus.Error("upgrade:", err)
		return
	}
	defer c.Close()
	for {
		deviceData := CameraConnection{}
		err := c.ReadJSON(&deviceData)
		if err != nil {
			logrus.Error("read:", err)
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
			logrus.Info("Device Connected: ", deviceName)

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
			logrus.Error("Device name conflicted :", deviceName)
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
				logrus.Error("Error write to DB: ", err)
				err = c.WriteJSON(ConnectionReply{ //device with same name exist on the server
					Status: false,
					Error:  "Error write to DB",
				})
				continue
			}
		}
		logrus.Info("device ready : ", deviceName)
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
	///define configuration name and load from it
	viper.SetConfigName("config")
	viper.SetConfigType("yaml")

	///define default value for config file
	viper.SetDefault("HttpPort", "8543")

	///read config from file
	err := viper.ReadInConfig() // Find and read the config file
	if err != nil {             // Handle errors reading the config file
		logrus.Warn("create new confg file: ./config.yaml")
		viper.WriteConfig()
	}

	//get router ip and set up upnp
	client, err := PickRouterClient(context.Background())
	if err != nil {

	}

	externalIP, err := client.GetExternalIPAddress()
	if err != nil {
		logrus.Warn("Cannot get external IP")
	} else {
		logrus.Info("external IP: ", externalIP)
	}

	internalIP, err := LocalIP()
	if err != nil {
		logrus.Error("Cannot get internal IP")
		logrus.Error("This server will only function locally")
	} else {
		logrus.Info("internal IP: ", internalIP.String())
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
			logrus.Error(err)
		}

		current := onlineDevice[p]
		//open port to wait for client stream if current track not exist
		if current.Track == nil {
			port := 40000 + rand.Intn(20000) //between 40000 - 60000
			err = errors.New("placeholder")
			for err != nil {
				current.Listener, err = net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("0.0.0.0"), Port: port})
				logrus.Error("Failed to open Port :", port)
			}
			logrus.Info("opened port for ", p, " on : ", port)

			//setup upnp port forwarding
			err = client.AddPortMapping(
				"",
				uint16(port),
				"UDP",
				uint16(port),
				string(internalIP),
				true,
				"WebRTC Stream for :"+p,
				3600,
			)
			if err != nil {
				logrus.Warn("failed to open external port " + strconv.Itoa(port) + " for: " + p)
			}

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
						logrus.Error(fmt.Sprintf("error during read: %s", err))
						continue
					}

					if _, err = current.Track.Write(inboundRTPPacket[:n]); err != nil {
						logrus.Error(current.Websocket.RemoteAddr(), err)
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
		logrus.Info("HTTP server started on: ", httpServePort)
		http.ListenAndServe(":"+string(httpServePort), nil)
	}()

	// start server and wait until a fatal error
	log.Printf("server is ready")
	a := make(chan bool) //keep main thread
	<-a
}
