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

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media/samplebuilder"
)

var videoTrack map[string]*webrtc.TrackLocalStaticSample

// This example shows how to
// 1. create a RTSP server which accepts plain connections
// 2. allow a single client to publish a stream with TCP or UDP
// 3. allow multiple clients to read that stream with TCP, UDP or UDP-multicast

/*
type serverHandler struct {
	mutex  sync.Mutex
	stream map[*gortsplib.ServerSession]struct {
		stream *gortsplib.ServerStream
		path   string
	}
	//publisher *gortsplib.ServerSession
}

// called when a connection is opened.
func (sh *serverHandler) OnConnOpen(ctx *gortsplib.ServerHandlerOnConnOpenCtx) {
	log.Printf("conn opened")
}

// called when a connection is closed.
func (sh *serverHandler) OnConnClose(ctx *gortsplib.ServerHandlerOnConnCloseCtx) {
	log.Printf("conn closed (%v)", ctx.Error)
}

// called when a session is opened.
func (sh *serverHandler) OnSessionOpen(ctx *gortsplib.ServerHandlerOnSessionOpenCtx) {
	log.Printf("session opened")
}

// called when a session is closed.
func (sh *serverHandler) OnSessionClose(ctx *gortsplib.ServerHandlerOnSessionCloseCtx) {
	log.Printf("session closed")
	sh.mutex.Lock()
	defer sh.mutex.Unlock()


		// if the session is the publisher,
		// close the stream and disconnect any reader.
		if sh.stream[ctx.pa] != nil && ctx.Session == sh.publisher {
			sh.stream.Close()
			sh.stream = nil
		}

	if val, ok := sh.stream[ctx.Session]; ok {
		val.stream.Close()
		delete(sh.stream, ctx.Session)
	}

}

/*
// called after receiving a DESCRIBE request.
func (sh *serverHandler) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	log.Printf("describe request")

	sh.mutex.Lock()
	defer sh.mutex.Unlock()

	// no one is publishing yet
	if sh.stream == nil {
		return &base.Response{
			StatusCode: base.StatusNotFound,
		}, nil, nil
	}

	// send the track list that is being published to the client
	return &base.Response{
		StatusCode: base.StatusOK,
	}, sh.stream, nil
}


// called after receiving an ANNOUNCE request.
func (sh *serverHandler) OnAnnounce(ctx *gortsplib.ServerHandlerOnAnnounceCtx) (*base.Response, error) {
	log.Printf("announce request")

	sh.mutex.Lock()
	defer sh.mutex.Unlock()

		if sh.stream != nil {
			return &base.Response{
				StatusCode: base.StatusBadRequest,
			}, fmt.Errorf("someone is already publishing")
		}

		// save the track list and the publisher
		sh.publisher = ctx.Session

	sh.stream[ctx.Session] = struct {
		stream *gortsplib.ServerStream
		path   string
	}{
		stream: gortsplib.NewServerStream(ctx.Tracks),
		path:   ctx.Path,
	}

	webrtc, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "video", "pion")
	if err != nil {
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, fmt.Errorf("allocate webrtc stream failed")
	}
	videoTrack[ctx.Path] = webrtc
	return &base.Response{
		StatusCode: base.StatusOK,
	}, nil

}


// called after receiving a SETUP request.
func (sh *serverHandler) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	log.Printf("setup request")
	log.Print(ctx.Path)


		// no one is publishing yet
		return &base.Response{
			StatusCode: base.StatusNotFound,
		}, nil, nil


	return &base.Response{
		StatusCode: base.StatusOK,
	}, sh.stream[ctx.Session].stream, nil

}

// called after receiving a PLAY request.
func (sh *serverHandler) OnPlay(ctx *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	log.Printf("play request")

	return &base.Response{
		StatusCode: base.StatusOK,
	}, nil
}

// called after receiving a RECORD request.
func (sh *serverHandler) OnRecord(ctx *gortsplib.ServerHandlerOnRecordCtx) (*base.Response, error) {
	log.Printf("record request")

	return &base.Response{
		StatusCode: base.StatusOK,
	}, nil
}

// called after receiving a RTP packet.
func (sh *serverHandler) OnPacketRTP(ctx *gortsplib.ServerHandlerOnPacketRTPCtx) {

	videoTrack[sh.stream[ctx.Session].path].WriteRTP(ctx.Packet)
}
*/

// Create a video track

func main() {
	//init
	videoTrack = map[string]*webrtc.TrackLocalStaticSample{}

	//first open a http server

	http.Handle("/", http.FileServer(http.Dir("./static")))

	// Open a UDP Listener for RTP Packets on port 5004
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("0.0.0.0"), Port: 5004})
	if err != nil {
		panic(err)
	}
	defer func() {
		if err = listener.Close(); err != nil {
			panic(err)
		}
	}()
	/*
		_, err = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "a")
		if err != nil {
			panic(err)
		}
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
					URLs: []string{"stun:stun.l.google.com:19302"},
				},
			},
		})
		if err != nil {
			panic(err)
		}

		rtpSender, err := peerConnection.AddTrack(videoTrack[p])
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
	go func() { http.ListenAndServe(":8543", nil) }()

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
