package main

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"log"

	"context"
	_ "expvar"
	"net"
	"net/http"
	_ "net/http/pprof"
	"strconv"
	"time"

	"github.com/RoaringBitmap/roaring"
	"github.com/coreos/etcd/client"
	"github.com/coreos/go-systemd/daemon"
	"github.com/davecgh/go-spew/spew"
	_ "github.com/go-sql-driver/mysql"
	"github.com/gorilla/mux"
	"github.com/inverse-inc/packetfence/go/pfconfigdriver"
	dhcp "github.com/krolaw/dhcp4"
	"github.com/patrickmn/go-cache"
)

var DHCPConfig *Interfaces
var database *sql.DB

var GlobalIpCache *cache.Cache
var GlobalMacCache *cache.Cache

// Control
var ControlOut map[string]chan interface{}
var ControlIn map[string]chan interface{}

var VIP map[string]bool
var VIPIp map[string]net.IP

var ctx = context.Background()

var Capi *client.Config

func main() {
	// Default http timeout
	http.DefaultClient.Timeout = 10 * time.Second

	// Initialize etcd config
	Capi = etcdInit()

	// Initialize IP cache
	GlobalIpCache = cache.New(5*time.Minute, 10*time.Minute)
	// Initialize Mac cache
	GlobalMacCache = cache.New(5*time.Minute, 10*time.Minute)

	// Read DB config
	configDatabase := readDBConfig()
	connectDB(configDatabase, database)

	database.SetMaxIdleConns(0)
	database.SetMaxOpenConns(500)

	VIP = make(map[string]bool)
	VIPIp = make(map[string]net.IP)

	go func() {
		var interfaces pfconfigdriver.ListenInts
		pfconfigdriver.FetchDecodeSocket(ctx, &interfaces)
		for {
			DHCPConfig.detectVIP(interfaces)

			time.Sleep(3 * time.Second)
		}
	}()

	// Read pfconfig
	DHCPConfig = newDHCPConfig()
	DHCPConfig.readConfig()

	// Queue value
	var (
		maxQueueSize = 100
		maxWorkers   = 50
	)

	ControlIn = make(map[string]chan interface{})
	ControlOut = make(map[string]chan interface{})

	// create job channel
	jobs := make(chan job, maxQueueSize)

	// create workers
	for i := 1; i <= maxWorkers; i++ {
		go func(i int) {
			for j := range jobs {
				doWork(i, j)
			}
		}(i)
	}

	// Unicast listener
	for _, v := range DHCPConfig.intsNet {
		v := v
		// Create a channel for each interfaces
		channelIn := make(chan interface{})
		channelOut := make(chan interface{})
		ControlIn[v.Name] = channelIn
		ControlOut[v.Name] = channelOut
		for net := range v.network {
			net := net
			go func() {
				v.runUnicast(jobs, v.network[net].dhcpHandler.ip)
			}()

			// We only need one listener per ip
			break
		}
	}

	// Broadcast listener
	for _, v := range DHCPConfig.intsNet {
		v := v
		go func() {
			v.run(jobs)
		}()
	}

	router := mux.NewRouter()
	router.HandleFunc("/help/", handleHelp).Methods("GET")
	router.HandleFunc("/mac2ip/{mac:(?:[0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}}", handleMac2Ip).Methods("GET")
	router.HandleFunc("/ip2mac/{ip:(?:[0-9]{1,3}.){3}(?:[0-9]{1,3})}", handleIP2Mac).Methods("GET")
	router.HandleFunc("/stats/{int:.*}", handleStats).Methods("GET")
	router.HandleFunc("/initialease/{int:.*}", handleInitiaLease).Methods("GET")
	router.HandleFunc("/debug/{int:.*}/{role:(?:[^/]*)}", handleDebug).Methods("GET")
	router.HandleFunc("/optionsnetwork/{network:(?:[0-9]{1,3}.){3}(?:[0-9]{1,3})}/{options:.*}", handleOverrideNetworkOptions).Methods("POST")
	router.HandleFunc("/options/{mac:(?:[0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}}/{options:.*}", handleOverrideOptions).Methods("POST")
	router.HandleFunc("/removeoptionsnetwork/{network:(?:[0-9]{1,3}.){3}(?:[0-9]{1,3})}", handleRemoveNetworkOptions).Methods("GET")
	router.HandleFunc("/removeoptions/{mac:(?:[0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}}", handleRemoveOptions).Methods("GET")
	router.HandleFunc("/releaseip/{mac:(?:[0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}}", handleReleaseIP).Methods("POST")

	// Api
	l, err := net.Listen("tcp", ":22222")
	if err != nil {
		log.Panicf("cannot listen: %s", err)
	}
	daemon.SdNotify(false, "READY=1")

	go func() {
		interval, err := daemon.SdWatchdogEnabled(false)
		if err != nil || interval == 0 {
			return
		}
		for {
			_, err := http.Get("http://127.0.0.1:22222")
			if err == nil {
				daemon.SdNotify(false, "WATCHDOG=1")
			}
			time.Sleep(interval / 3)
		}
	}()
	http.Serve(l, router)
}

// Broadcast runner
func (h *Interface) run(jobs chan job) {

	// Communicate with the server that run on an interface
	go func() {
		inchannel := ControlIn[h.Name]
		outchannel := ControlOut[h.Name]
		for {

			Request := <-inchannel
			stats := make(map[string]Stats)
			// Send back stats
			if Request.(ApiReq).Req == "stats" {
				for _, v := range h.network {
					var statistiques roaring.Statistics
					statistiques = v.dhcpHandler.available.Stats()
					var Options map[string]string
					Options = make(map[string]string)
					Options["OptionIPAddressLeaseTime"] = v.dhcpHandler.leaseDuration.String()
					for option, value := range v.dhcpHandler.options {
						Options[option.String()] = Tlv.Tlvlist[int(option)].Decode.String(value)
					}

					// Add network options on the fly
					x, err := decodeOptions(v.network.IP.String())
					if err {
						for key, value := range x {
							Options[key.String()] = Tlv.Tlvlist[int(key)].Decode.String(value)
						}
					}

					var Members map[string]string
					Members = make(map[string]string)
					members := v.dhcpHandler.hwcache.Items()
					var Status string
					var Count int
					Count = 0
					for i, item := range members {
						Count++
						result := make(net.IP, 4)
						binary.BigEndian.PutUint32(result, binary.BigEndian.Uint32(v.dhcpHandler.start.To4())+uint32(item.Object.(int)))
						Members[i] = result.String()
					}

					if Count == (v.dhcpHandler.leaseRange - int(statistiques.RunContainerValues)) {
						Status = "Normal"
					} else {
						Status = "Calculated available IP " + strconv.Itoa(v.dhcpHandler.leaseRange-Count) + " is different than what we have available in the pool " + strconv.Itoa(int(statistiques.RunContainerValues))
					}

					stats[v.network.String()] = Stats{EthernetName: Request.(ApiReq).NetInterface, Net: v.network.String(), Free: int(statistiques.RunContainerValues), Category: v.dhcpHandler.role, Options: Options, Members: Members, Status: Status}
				}
				outchannel <- stats
			}
			// Update the lease
			if Request.(ApiReq).Req == "initialease" {

				for _, v := range h.network {
					initiaLease(&v.dhcpHandler)
					stats[v.network.String()] = Stats{EthernetName: Request.(ApiReq).NetInterface, Net: v.network.String(), Category: v.dhcpHandler.role, Status: "Init Lease success"}
				}
				outchannel <- stats
			}

			if Request.(ApiReq).Req == "debug" {
				for _, v := range h.network {
					if Request.(ApiReq).Role == v.dhcpHandler.role {
						spew.Dump(v.dhcpHandler.available.Stats())
						fmt.Println(v.dhcpHandler.available.String())
						stats[v.network.String()] = Stats{EthernetName: Request.(ApiReq).NetInterface, Net: v.network.String(), Category: v.dhcpHandler.role, Status: "Debug finished"}
					}
				}
				outchannel <- stats
			}
		}
	}()
	ListenAndServeIf(h.Name, h, jobs)

}

// Unicast runner
func (h *Interface) runUnicast(jobs chan job, ip net.IP) {

	ListenAndServeIfUnicast(h.Name, h, jobs, ip)
}

func (h *Interface) ServeDHCP(p dhcp.Packet, msgType dhcp.MessageType, options dhcp.Options) (answer Answer) {

	var handler DHCPHandler
	var NetScope net.IPNet

	answer.MAC = p.CHAddr()
	answer.SrcIP = h.Ipv4

	// Detect the handler to use (config)
	var NodeCache *cache.Cache
	NodeCache = cache.New(3*time.Second, 5*time.Second)
	var node NodeInfo
	for _, v := range h.network {

		// Case of a l2 dhcp request
		// Network is L2 and (relais IP is null or
		if v.dhcpHandler.layer2 && (p.GIAddr().Equal(net.IPv4zero) || v.network.Contains(p.CIAddr())) {

			// Ip per role ?
			if v.splittednet == true {

				if x, found := NodeCache.Get(p.CHAddr().String()); found {
					node = x.(NodeInfo)
				} else {
					node = NodeInformation(p.CHAddr())
					NodeCache.Set(p.CHAddr().String(), node, 3*time.Second)
				}

				var category string
				var nodeinfo = node
				// Undefined role then use the registration one
				if nodeinfo.Category == "" || nodeinfo.Status == "unreg" {
					category = "registration"
				} else {
					category = nodeinfo.Category
				}

				if v.dhcpHandler.role == category {
					handler = v.dhcpHandler
					NetScope = v.network
					answer.SrcIP = handler.ip
					break
				}
				continue
			} else {
				// Case we are in L3
				if !p.CIAddr().Equal(net.IPv4zero) && !v.network.Contains(p.CIAddr()) {
					continue
				}
				handler = v.dhcpHandler
				NetScope = v.network
				break
			}
		}
		// Case dhcprequest from an already assigned l3 ip address
		if p.GIAddr().Equal(net.IPv4zero) && v.network.Contains(p.CIAddr()) {
			handler = v.dhcpHandler
			NetScope = v.network
			break
		}

		if (!p.GIAddr().Equal(net.IPv4zero) && v.network.Contains(p.GIAddr())) || v.network.Contains(p.CIAddr()) {
			handler = v.dhcpHandler
			NetScope = v.network
			break
		}
	}

	if len(handler.ip) == 0 {
		return answer
	}
	// Do we have the vip ?

	if VIP[h.Name] {

		switch msgType {

		case dhcp.Discover:
			fmt.Println(p.CHAddr().String() + " " + msgType.String())
			var free int
			i := handler.available.Iterator()

			// Search in the cache if the mac address already get assigned
			if x, found := handler.hwcache.Get(p.CHAddr().String()); found {
				free = x.(int)
				// 5 seconds to send a request
				handler.hwcache.Set(p.CHAddr().String(), free, time.Duration(5)*time.Second)
				goto reply
			}

			// Search for the next available ip in the pool
		retry:
			if i.HasNext() {
				element := i.Next()
				handler.available.Remove(element)
				free = int(element)
				// Ping the ip address
				pingreply := Ping(dhcp.IPAdd(handler.start, free).String(), 1)
				if pingreply {
					fmt.Println(p.CHAddr().String() + " Ip " + dhcp.IPAdd(handler.start, free).String() + " already in use, trying next")
					handler.available.Add(element)
					goto retry
				}
				handler.available.Remove(element)
				// 5 seconds to send a request
				handler.hwcache.Set(p.CHAddr().String(), free, time.Duration(5)*time.Second)
			} else {
				fmt.Println(p.CHAddr().String() + " Nak No space left in the pool ")
				return answer
			}

		reply:

			answer.IP = dhcp.IPAdd(handler.start, free)
			answer.Iface = h.intNet
			// Add options on the fly
			var GlobalOptions dhcp.Options
			var options = make(map[dhcp.OptionCode][]byte)
			for key, value := range handler.options {
				if key == dhcp.OptionDomainNameServer || key == dhcp.OptionRouter {
					options[key] = ShuffleIP(value)
				} else {
					options[key] = value
				}
			}
			GlobalOptions = options
			leaseDuration := handler.leaseDuration

			// Add network options on the fly
			x, err := decodeOptions(NetScope.IP.String())
			if err {
				for key, value := range x {
					if key == dhcp.OptionIPAddressLeaseTime {
						seconds, _ := strconv.Atoi(string(value))
						leaseDuration = time.Duration(seconds) * time.Second
						continue
					}
					GlobalOptions[key] = value
				}
			}

			// Add device (mac) options on the fly
			x, err = decodeOptions(p.CHAddr().String())
			if err {
				for key, value := range x {
					if key == dhcp.OptionIPAddressLeaseTime {
						seconds, _ := strconv.Atoi(string(value))
						leaseDuration = time.Duration(seconds) * time.Second
						continue
					}
					GlobalOptions[key] = value
				}
			}
			fmt.Println(p.CHAddr().String() + " Offer " + answer.IP.String())
			answer.D = dhcp.ReplyPacket(p, dhcp.Offer, handler.ip.To4(), answer.IP, leaseDuration,
				GlobalOptions.SelectOrderOrAll(options[dhcp.OptionParameterRequestList]))

			return answer

		case dhcp.Request:
			// Some client will not send OptionServerIdentifier
			// if server, ok := options[dhcp.OptionServerIdentifier]; ok && (!net.IP(server).Equal(h.Ipv4) && !net.IP(server).Equal(handler.ip)) {
			// 	return answer // Message not for this dhcp server
			// }

			reqIP := net.IP(options[dhcp.OptionRequestedIPAddress])
			if reqIP == nil {
				reqIP = net.IP(p.CIAddr())
			}
			fmt.Println(p.CHAddr().String() + " " + msgType.String() + " " + reqIP.String())

			answer.IP = reqIP
			answer.Iface = h.intNet

			if len(reqIP) == 4 && !reqIP.Equal(net.IPv4zero) {
				if leaseNum := dhcp.IPRange(handler.start, reqIP) - 1; leaseNum >= 0 && leaseNum < handler.leaseRange {
					if index, found := handler.hwcache.Get(p.CHAddr().String()); found {
						if dhcp.IPAdd(handler.start, index.(int)).Equal(reqIP) {

							var GlobalOptions dhcp.Options
							var options = make(map[dhcp.OptionCode][]byte)
							for key, value := range handler.options {
								if key == dhcp.OptionDomainNameServer || key == dhcp.OptionRouter {
									options[key] = ShuffleIP(value)
								} else {
									options[key] = value
								}
							}
							GlobalOptions = options
							leaseDuration := handler.leaseDuration

							// Add network options on the fly
							x, err := decodeOptions(NetScope.IP.String())
							if err {
								for key, value := range x {
									if key == dhcp.OptionIPAddressLeaseTime {
										seconds, _ := strconv.Atoi(string(value))
										leaseDuration = time.Duration(seconds) * time.Second
										continue
									}
									GlobalOptions[key] = value
								}
							}

							// Add devices options on the fly
							x, err = decodeOptions(p.CHAddr().String())
							if err {
								for key, value := range x {
									if key == dhcp.OptionIPAddressLeaseTime {
										seconds, _ := strconv.Atoi(string(value))
										leaseDuration = time.Duration(seconds) * time.Second
										continue
									}
									GlobalOptions[key] = value
								}
							}

							answer.D = dhcp.ReplyPacket(p, dhcp.ACK, handler.ip.To4(), reqIP, leaseDuration,
								GlobalOptions.SelectOrderOrAll(options[dhcp.OptionParameterRequestList]))
							// Update Global Caches
							GlobalIpCache.Set(reqIP.String(), p.CHAddr().String(), leaseDuration+(time.Duration(15)*time.Second))
							GlobalMacCache.Set(p.CHAddr().String(), reqIP.String(), leaseDuration+(time.Duration(15)*time.Second))
							// Update the cache
							fmt.Println(p.CHAddr().String() + " Ack " + reqIP.String())

							handler.hwcache.Set(p.CHAddr().String(), index, leaseDuration+(time.Duration(15)*time.Second))
							return answer
						} else {
							fmt.Println(p.CHAddr().String() + " Asked for an IP " + reqIP.String() + " that hasnt been assigned by Offer " + dhcp.IPAdd(handler.start, index.(int)).String())
							// pingreply := Ping(reqIP.String(), 1)
							// if pingreply {
							// 	fmt.Println(p.CHAddr().String() + " Ip " + reqIP.String() + " already in use")
							// }
						}
					}
				}
			}
			fmt.Println(p.CHAddr().String() + " Nak")
			answer.D = dhcp.ReplyPacket(p, dhcp.NAK, handler.ip.To4(), nil, 0, nil)

		case dhcp.Release, dhcp.Decline:

			if x, found := handler.hwcache.Get(p.CHAddr().String()); found {
				handler.available.Add(uint32(x.(int)))
				handler.hwcache.Delete(p.CHAddr().String())
			}
		}
		return answer
	}
	answer.Iface = h.intNet
	fmt.Println(p.CHAddr().String() + " Nak")
	answer.D = dhcp.ReplyPacket(p, dhcp.NAK, handler.ip.To4(), nil, 0, nil)
	return answer
}
