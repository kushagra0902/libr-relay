package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"io"

	//"io"
	"math/big"
	"sort"
	"strings"
	"sync"

	//Peers "chatprotocol/peer"

	"context"
	//"encoding/csv"

	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"net/http"

	//"github.com/joho/godotenv"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/p2p/net/connmgr"
	relay "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	libp2ptls "github.com/libp2p/go-libp2p/p2p/security/tls"
	"github.com/libp2p/go-libp2p/p2p/transport/tcp"
	"github.com/libp2p/go-libp2p/p2p/transport/websocket"
	ma "github.com/multiformats/go-multiaddr"

	"go.mongodb.org/mongo-driver/mongo"
)

type RelayDist struct {
	relayID string
	dist    *big.Int
}

const ChatProtocol = protocol.ID("/chat/1.0.0")

//var RelayMultiAddrList = []string{"/dns4/0.tcp.in.ngrok.io/tcp/14395/p2p/12D3KooWLBVV1ty7MwJQos34jy1WqGrfkb3bMAfxUJzCgwTBQ2pn",}

type reqFormat struct {
	Type string `json:"type,omitempty"`
	//PubIP     string          `json:"pubip,omitempty"`
	PeerID    string          `json:"peer_id"`
	ReqParams json.RawMessage `json:"reqparams,omitempty"`
	Body      json.RawMessage `json:"body,omitempty"`
}

// var (
// 	IDmap = make(map[string]string)
// 	mu    sync.RWMutex
// )

var (
	ConnectedPeers []string
	mu             sync.RWMutex
	JS_ServerURL   string
	JS_API_key     string
)

var RelayHost host.Host

var (
	MongoClient *mongo.Client
	ctx         context.Context
	cancel      context.CancelFunc
)

type respFormat struct {
	Type string `json:"type"`
	Resp []byte `json:"resp"`
}

type RelayEvents struct{}

var OwnRelayAddrFull string

func (re *RelayEvents) Listen(net network.Network, addr ma.Multiaddr)      {}
func (re *RelayEvents) ListenClose(net network.Network, addr ma.Multiaddr) {}
func (re *RelayEvents) Connected(net network.Network, conn network.Conn) {
	fmt.Printf("[INFO] Peer connected: %s\n", conn.RemotePeer())
}
func (re *RelayEvents) Disconnected(net network.Network, conn network.Conn) {
	fmt.Printf("[INFO] Peer disconnected: %s\n", conn.RemotePeer())
	// Remove peer from IDmap if needed
	mu.Lock()
	// for pubip, pid := range IDmap {
	// 	if pid == conn.RemotePeer().String() {
	// 		delete(IDmap, pubip)
	// 		break
	// 	}
	// }
	if contains(ConnectedPeers, conn.RemotePeer().String()) {
		remove(&ConnectedPeers, conn.RemotePeer().String())
	}
	mu.Unlock()
}

func main() {
	fmt.Println("STARTING RELAY CODE")
	//godotenv.Load()
	JS_API_key = os.Getenv("JS_API_key")
	JS_ServerURL = os.Getenv("JS_ServerURL")
	if JS_API_key == "" || JS_ServerURL == "" {
		fmt.Println("[DEBUG] Missing JS API key or server URL")
		return
	}
	// fmt.Println(mongo_uri)

	fmt.Println("[DEBUG] Creating connection manager...")
	connMgr, err := connmgr.NewConnManager(100, 400)
	if err != nil {
		log.Fatalf("[ERROR] Failed to create connection manager: %v", err)
	}

	privKey, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		// handle error
		panic(err)
	}
	fmt.Println("[DEBUG] Creating relay host...")

	RelayHost, err = libp2p.New(
		libp2p.Identity(privKey),

		libp2p.ListenAddrStrings("/ip4/0.0.0.0/tcp/443/ws"),
		libp2p.Security(libp2ptls.ID, libp2ptls.New),
		libp2p.ConnectionManager(connMgr),
		libp2p.EnableNATService(),
		libp2p.EnableRelayService(),
		libp2p.Transport(tcp.NewTCPTransport),
		libp2p.Transport(websocket.New),
	)
	if err != nil {
		log.Fatalf("[ERROR] Failed to create relay host: %v", err)
	}

	// pubKey := privKey.GetPublic()

	// pubKeyBytes, err := crypto.MarshalPublicKey(pubKey)
	// if err != nil {
	// 	log.Fatalf("Failed to marshal public key: %v", err)
	// }

	// pubKeyStr := base64.StdEncoding.EncodeToString(pubKeyBytes)

	RelayHost.Network().Notify(&RelayEvents{})

	OwnRelayAddrFull = fmt.Sprintf("/dns4/relay-8wrh.onrender.com/tcp/443/wss/p2p/%s", RelayHost.ID().String())
	err = ConnectJSServer()
	if err != nil {
		fmt.Printf("[DEBUG] Error connecting to JS server: %s\n", err)
		return
	}

	customRelayResources := relay.Resources{
		ReservationTTL:         time.Hour,
		MaxReservations:        1000,
		MaxCircuits:            64,
		BufferSize:             64 * 1024,
		MaxReservationsPerPeer: 10,
		MaxReservationsPerIP:   400,
		MaxReservationsPerASN:  64,
	}

	// Enable circuit relay service
	fmt.Println("[DEBUG] Enabling circuit relay service...")
	_, err = relay.New(RelayHost, relay.WithResources(customRelayResources))
	if err != nil {
		log.Fatalf("[ERROR] Failed to enable relay service: %v", err)
	}

	fmt.Printf("[INFO] Relay started!\n")
	fmt.Printf("[INFO] Peer ID: %s\n", RelayHost.ID())

	// Print all addresses
	for _, addr := range RelayHost.Addrs() {
		fmt.Printf("[INFO] Relay Address: %s/p2p/%s\n", addr, RelayHost.ID())
	}

	//OwnRelayAddrFull =  fmt.Sprintf("/dns4/0.tcp.in.ngrok.io/tcp/%s/p2p/%s","port_number", RelayHost.ID().String())

	//go uploadRelayAddrToSheet(relayMultiaddrFull)

	RelayHost.SetStreamHandler("/chat/1.0.0", handleChatStream)
	go func() {
		for {
			fmt.Println(ConnectedPeers)
			time.Sleep(5 * time.Second)
		}
	}()

	addr, _ := GetRelayAddrFromJSServer()
	go PingTargets(addr, 5*time.Minute)

	fmt.Println("[DEBUG] Waiting for interrupt signal...")
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c
	deleteFromJSServer()
	fmt.Println("[INFO] Shutting down relay...")
}
func remove(Lists *[]string, val string) {
	for i, item := range *Lists {
		if item == val {
			*Lists = append((*Lists)[:i], (*Lists)[i+1:]...)
			return
		}
	}
}

func PingTargets(addresses []string, interval time.Duration) {
	go func() {

		for {
			for _, multiAddrStr := range addresses {
				// Parse the multiaddress string
				maddr, err := ma.NewMultiaddr(multiAddrStr)
				if err != nil {
					log.Printf("[WARN] Could not parse multiaddress %s: %v\n", multiAddrStr, err)
					continue
				}

				// Extract the domain name
				host, err := maddr.ValueForProtocol(ma.P_DNS4)
				if err != nil {
					// Fallback for P_DNS6 or other domain protocols if needed
					host, err = maddr.ValueForProtocol(ma.P_DNS6)
					if err != nil {
						log.Printf("[WARN] Could not extract host from multiaddress %s: %v\n", multiAddrStr, err)
						continue
					}
				}

				// Construct the final HTTP URL for the health check
				pingURL := fmt.Sprintf("https://%s/check", host)

				// Ping the valid URL
				resp, err := http.Get(pingURL)
				if err != nil {
					log.Printf("[WARN] Failed to ping %s: %v\n", pingURL, err)
					continue
				}
				resp.Body.Close()

				//Ping the js server

				resp2,err := http.Get("https://libr-server.onrender.com/")
				if err != nil {
					log.Printf("[WARN] Failed to ping %s:","https://libr-server.onrender.com/")
					continue
				}
				resp2.Body.Close()

				log.Printf("[INFO] Pinged %s — Status: %s\n", pingURL, resp.Status)
			}
			time.Sleep(interval)
		}
	}()
}

func contains(arr []string, target string) bool {
	for _, vals := range arr {
		if vals == target {
			return true
		}
	}
	return false
}

func handleChatStream(s network.Stream) {
	fmt.Println("[DEBUG] Incoming chat stream from", s.Conn().RemoteMultiaddr())
	defer s.Close()
	//reader := bufio.NewReader(s)
	decoder := json.NewDecoder(s)

	for {

		var req reqFormat
		err := decoder.Decode(&req)
		if err != nil {
			// io.EOF means the other side closed the connection cleanly.
			if err != io.EOF {
				fmt.Printf("[DEBUG] Error decoding JSON at relay: %v\n", err)
			}
			return // Exit the loop on any error or clean disconnect.
		}
		//buf := make([]byte, 1024*64) // or size based on expected message
		//n, err := reader.Read(buf)
		//fmt.Println(n)
		// if err != nil {
		// 	fmt.Println("[DEBUG] Error reading from connection at relay:", err)
		// 	return
		// }
		//buf = bytes.TrimRight(buf, "\x00")

		//err = json.Unmarshal(buf[:n], &req)
		// if err != nil {
		// 	fmt.Printf("[DEBUG] Error parsing JSON at relay: %v\n", err)
		// 	fmt.Printf("[DEBUG] Received Data: %s\n", string(buf[:n]))
		// 	return
		// }

		fmt.Printf("req by user is : %+v \n", req)

		if req.Type == "register" {
			peerID := s.Conn().RemotePeer()
			peerID2 := req.PeerID

			if peerID2 != peerID.String() {
				fmt.Println("PEER ID MISMATCH")
				return
			}

			fmt.Printf("[INFO]Given peerID is %s \n", req.PeerID)
			fmt.Println("[INFO]Registering the peer into relay")
			mu.Lock()
			//IDmap[req.PubIP] = peerID.String()
			ConnectedPeers = append(ConnectedPeers, peerID.String())
			mu.Unlock()
		}

		if req.Type == "SendMsg" {
			mu.RLock()
			var targetPeerID string

			if contains(ConnectedPeers, req.PeerID) {
				targetPeerID = req.PeerID
			}
			mu.RUnlock()

			// checks if the target peer is connected to the relay or some other relay
			// have to handle some logic here but later

			if targetPeerID == "" {
				fmt.Println("[DEBUG]This peer is not on this relay, contacting other relay")
				targetRelayAddr := GetRelayAddr(req.PeerID)
				if targetRelayAddr == "" {
					fmt.Println("Can't get relay addr from mongoDB")
					s.Write([]byte("[DEBUG]Can't get Relay addresses from database, retry again"))
					return
				}
				if targetRelayAddr == OwnRelayAddrFull {
					s.Write([]byte("[DEBUG]Target Peer not in network"))
					return
				}
				var forwardReq reqFormat
				forwardReq.Body = req.Body
				forwardReq.ReqParams = req.ReqParams
				forwardReq.PeerID = req.PeerID
				forwardReq.Type = "forward"

				relayMA, err := ma.NewMultiaddr(targetRelayAddr)
				if err != nil {
					fmt.Println("[DEBUG] Failed to parse relay multiaddr:", err)
					return
				}

				TargetRelayInfo, err := peer.AddrInfoFromP2pAddr(relayMA)
				if err != nil {
					fmt.Println("[DEBUG] Failed to parse target relay info:", err)
					return
				}

				err = RelayHost.Connect(context.Background(), *TargetRelayInfo)
				if err != nil {
					fmt.Println("[DEBUG] Failed to connect to target relay:", err)
					return
				}

				forwardStream, err := RelayHost.NewStream(context.Background(), TargetRelayInfo.ID, ChatProtocol)
				if err != nil {
					fmt.Println("[DEBUG] Failed to open stream to target relay:", err)
					return
				}
				defer forwardStream.Close()

				encoder := json.NewEncoder(forwardStream)
				if err := encoder.Encode(forwardReq); err != nil {
					fmt.Println("[DEBUG] Failed to write forward request to stream:", err)
					return
				}

				// Read the response back using a decoder
				var respBody json.RawMessage
				respDecoder := json.NewDecoder(forwardStream)
				if err := respDecoder.Decode(&respBody); err != nil {
					fmt.Println("[DEBUG] Error reading response from target relay:", err)
					return
				}

				fmt.Printf("[Debug]Frowarded Resp from relay : %s : %s \n", TargetRelayInfo.ID.String(), string(respBody))

				_, err = s.Write(respBody)
				if err != nil {
					fmt.Println("[DEBUG] Error sending back to original sender:", err)
					return
				}

			} else {
				fmt.Println("Target peer ID: ", targetPeerID)
				if RelayHost == nil {
					fmt.Println("[FATAL] RelayHost is nil!")
					return
				}

				targetID, err := peer.Decode(targetPeerID)
				if err != nil {
					log.Printf("[ERROR] Invalid Peer ID: %v", err)
					s.Write([]byte("invalid peer id"))
					return
				}

				relayBaseAddr, err := ma.NewMultiaddr("/p2p/" + RelayHost.ID().String())
				if err != nil {
					log.Fatal("relayBaseAddr error:", err)
				}
				circuitAddr, _ := ma.NewMultiaddr("/p2p-circuit")
				targetAddr, _ := ma.NewMultiaddr("/p2p/" + targetID.String())
				fullAddr := relayBaseAddr.Encapsulate(circuitAddr).Encapsulate(targetAddr)
				fmt.Println("[DEBUG]", fullAddr.String())

				addrInfo, err := peer.AddrInfoFromP2pAddr(fullAddr)
				if err != nil {
					log.Printf("Invalid relayed multiaddr: %s", fullAddr)
					s.Write([]byte("bad relayed addr"))
					return
				}

				RelayHost.Peerstore().AddAddrs(addrInfo.ID, addrInfo.Addrs, peerstore.PermanentAddrTTL)

				err = RelayHost.Connect(context.Background(), *addrInfo)
				if err != nil {
					log.Printf("[ERROR] Failed to connect to relayed peer: %v", err)
				}

				sendStream, err := RelayHost.NewStream(context.Background(), targetID, ChatProtocol)
				if err != nil {
					fmt.Println("[DEBUG]Error opening stream to target peer", err)
					s.Write([]byte("failed"))
					return
				}
				defer sendStream.Close()

				encoder := json.NewEncoder(sendStream)
				if err := encoder.Encode(req); err != nil {
					fmt.Println("[DEBUG]Error sending message despite stream opened:", err)
					return
				}

				var respBody json.RawMessage
				respDecoder := json.NewDecoder(sendStream)
				if err := respDecoder.Decode(&respBody); err != nil {
					fmt.Println("[DEBUG]Error reading response from target peer:", err)
					return
				}

				fmt.Printf("[Debug]Resp from %s : %s \n", targetID.String(), string(respBody))
				fmt.Println("[DEBUG]Raw Resp :", string(respBody))

				_, err = s.Write(respBody)
				if err != nil {
					fmt.Println("[DEBUG]Error sending response back:", err)
				}
			}
		}
		if req.Type == "forward" {
			mu.RLock()
			var targetPeerID string
			if contains(ConnectedPeers, req.PeerID) {
				targetPeerID = req.PeerID
			}
			mu.RUnlock()

			if targetPeerID == "" {
				fmt.Println("[DEBUG] Target peer not found in this relay")
				s.Write([]byte("Target peer not found"))
				return
			}

			targetID, err := peer.Decode(targetPeerID)
			if err != nil {
				fmt.Println("[DEBUG] Invalid target peer ID")
				return
			}

			relayID := RelayHost.ID()
			relayBaseAddr, _ := ma.NewMultiaddr("/p2p/" + relayID.String())
			circuitAddr, _ := ma.NewMultiaddr("/p2p-circuit")
			targetAddr, _ := ma.NewMultiaddr("/p2p/" + targetID.String())
			fullAddr := relayBaseAddr.Encapsulate(circuitAddr).Encapsulate(targetAddr)

			addrInfo, err := peer.AddrInfoFromP2pAddr(fullAddr)
			if err != nil {
				fmt.Println("[DEBUG] Invalid relayed address")
				return
			}

			RelayHost.Peerstore().AddAddrs(addrInfo.ID, addrInfo.Addrs, peerstore.PermanentAddrTTL)

			err = RelayHost.Connect(context.Background(), *addrInfo)
			if err != nil {
				fmt.Println("[DEBUG] Failed to connect to target peer at this relay")
				return
			}

			sendStream, err := RelayHost.NewStream(context.Background(), targetID, ChatProtocol)
			if err != nil {
				fmt.Println("[DEBUG] Failed to open stream to target peer")
				return
			}
			defer sendStream.Close()

			encoder := json.NewEncoder(sendStream)
			if err := encoder.Encode(req); err != nil {
				fmt.Println("[DEBUG]Error sending message despite stream opened:", err)
				return
			}

			var respBody json.RawMessage
			respDecoder := json.NewDecoder(sendStream)
			if err := respDecoder.Decode(&respBody); err != nil {
				fmt.Println("[DEBUG]Error reading forwarded response from target peer:", err)
				return
			}
			fmt.Printf("[Debug]Resp from %s : %s \n", targetID.String(), string(respBody))
			fmt.Println("[DEBUG]Raw Resp :", string(respBody))

			_, err = s.Write(respBody)
			if err != nil {
				fmt.Println("[DEBUG]Error sending response back:", err)
			}
		}
	}
}

func GetRelayAddr(peerID string) string {
	RelayMultiAddrList, err := GetRelayAddrFromJSServer()

	if err != nil {
		fmt.Println("[DEBUG]Error getting from JS server: ", err)
		return ""
	}
	var relayList []string
	for _, multiaddr := range RelayMultiAddrList {
		if multiaddr == OwnRelayAddrFull {
			continue
		}
		parts := strings.Split(multiaddr, "/")
		relayList = append(relayList, parts[len(parts)-1])
	}

	var distmap []RelayDist

	h1 := sha256.New() // Use sha256.New() for SHA-256
	h1.Write([]byte(peerID))
	peerIDhash := hex.EncodeToString(h1.Sum(nil))

	for _, relay := range relayList {

		h_R := sha256.New() // Use sha256.New() for SHA-256
		h_R.Write([]byte(relay))
		RelayIDhash := hex.EncodeToString(h_R.Sum(nil))

		dist := XorHexToBigInt(peerIDhash, RelayIDhash)

		distmap = append(distmap, RelayDist{dist: dist, relayID: relay})
	}

	sort.Slice(distmap, func(i, j int) bool {
		return distmap[i].dist.Cmp(distmap[j].dist) < 0
	})

	relayIDused := distmap[0].relayID

	var relayAddr string

	for _, multiaddr := range RelayMultiAddrList {
		parts := strings.Split(multiaddr, "/")
		if parts[len(parts)-1] == relayIDused {
			relayAddr = multiaddr
			break
		}
	}

	return relayAddr
}

func XorHexToBigInt(hex1, hex2 string) *big.Int {

	bytes1, err1 := hex.DecodeString(hex1)
	bytes2, err2 := hex.DecodeString(hex2)

	if err1 != nil || err2 != nil {
		log.Fatalf("Error decoding hex: %v %v", err1, err2)
	}

	if len(bytes1) != len(bytes2) {
		log.Fatalf("Hex strings must be the same length")
	}

	xorBytes := make([]byte, len(bytes1))
	for i := 0; i < len(bytes1); i++ {
		xorBytes[i] = bytes1[i] ^ bytes2[i]
	}

	result := new(big.Int).SetBytes(xorBytes)
	return result
}

// func AddRelayAddrToCSV(myAddr string, path string) error {
// 	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
// 	if err != nil {
// 		return err
// 	}
// 	defer f.Close()
// 	_, err = f.WriteString(myAddr + "\n")
// 	return err
// }

// func SetupMongo(uri string) error {
// 	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
// 	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
// 	if err != nil {
// 		cancel()
// 		return err
// 	}

// 	// Check connection
// 	if err := client.Ping(ctx, nil); err != nil {
// 		cancel()
// 		return err
// 	}

// 	MongoClient = client
// 	log.Println("ongoDB connected")
// 	return nil
// }

// func SetupMongo(uri string, PeerID string, PublicKey string) error {
// 	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
// 	defer cancel()

// 	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
// 	if err != nil {
// 		return fmt.Errorf("failed to connect to MongoDB: %w", err)
// 	}

// 	if err := client.Ping(ctx, nil); err != nil {
// 		return fmt.Errorf("failed to ping MongoDB: %w", err)
// 	}

// 	MongoClient = client

// 	collection := MongoClient.Database("Addrs").Collection("nodes")

// 	newNode := bson.D{
// 		{Key: "peer_id", Value: PeerID},
// 		{Key: "public_key", Value: PublicKey},
// 	}

// 	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
// 	defer cancel()

// 	_, err = collection.InsertOne(ctx, newNode)
// 	if err != nil {
// 		return fmt.Errorf("failed to insert document: %w", err)
// 	}

// 	log.Printf("Successfully inserted node with peer_id: %s", PeerID)
// 	return nil
// }

func ConnectJSServer() error {
	postData := map[string]string{
		"address": OwnRelayAddrFull,
	}

	jsonData, err := json.Marshal(postData)
	if err != nil {
		return fmt.Errorf("failed to marshal JSON: %w", err)
	}

	req, err := http.NewRequest("POST", JS_ServerURL+"/api/postrelay", bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-Key", JS_API_key)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned non-200 status code: %d", resp.StatusCode)
	}

	fmt.Printf("Successfully connected to JS server:")
	return nil
}

type ResponsePayload struct {
	BootList []Relay `json:"boot_list"`
}

type Relay struct {
	Address string `json:"address"`
}

func GetRelayAddrFromJSServer() ([]string, error) {
	req, err := http.NewRequest("GET", JS_ServerURL+"/api/getrelay", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server returned non-200 status code: %d", resp.StatusCode)
	}

	var payload ResponsePayload
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("failed to decode JSON response: %w", err)
	}

	var addresses []string
	for _, relay := range payload.BootList {
		addresses = append(addresses, relay.Address)
	}

	return addresses, nil
}

func deleteFromJSServer() error {
	deleteData := map[string]string{
		"address": OwnRelayAddrFull,
	}

	jsonData, err := json.Marshal(deleteData)
	if err != nil {
		return fmt.Errorf("failed to marshal JSON: %w", err)
	}

	req, err := http.NewRequest("DELETE", JS_ServerURL+"/api/deleterelay", bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", JS_API_key)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned non-200 status code: %d", resp.StatusCode)
	}

	fmt.Printf("Successfully deleted relay")
	return nil
}

// func GetRelayAddrFromMongo() ([]string, error) {
// 	collection := MongoClient.Database("Addrs").Collection("relays") // replace with actual DB & collection
// 	cursor, err := collection.Find(ctx, bson.M{})
// 	if err != nil {
// 		return nil, err
// 	}
// 	defer cursor.Close(ctx)

// 	var relayList []string
// 	for cursor.Next(ctx) {
// 		var doc struct {
// 			Address string `bson:"address"`
// 		}
// 		if err := cursor.Decode(&doc); err != nil {
// 			return nil, err
// 		}
// 		if strings.HasPrefix(doc.Address, "/") {
// 			relayList = append(relayList, strings.TrimSpace(doc.Address))
// 		}
// 	}
// 	return relayList, nil
// }

// func GetRelayAddrFromMongo() ([]string, error) {
// 	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
// 	defer cancel()

// 	collection := MongoClient.Database("Addrs").Collection("relays")
// 	cursor, err := collection.Find(ctx, bson.M{})
// 	if err != nil {
// 		return nil, fmt.Errorf("failed to fetch relay addresses: %w", err)
// 	}
// 	defer cursor.Close(ctx)

// 	var relayList []string
// 	for cursor.Next(ctx) {
// 		var doc struct {
// 			Address string `bson:"address"`
// 		}
// 		if err := cursor.Decode(&doc); err != nil {
// 			return nil, fmt.Errorf("failed to decode relay document: %w", err)
// 		}
// 		if strings.HasPrefix(doc.Address, "/") {
// 			relayList = append(relayList, strings.TrimSpace(doc.Address))
// 		}
// 	}

// 	return relayList, nil
// }