package server

import (
	"bepass/doh"
	"bepass/socks5"
	"bepass/socks5/statute"
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/ameshkov/dnscrypt/v2"
	"github.com/jellydator/ttlcache/v3"
	"github.com/miekg/dns"
)

type Server struct {
	TLSHeaderLength       int
	DnsCacheTTL           int
	RemoteDNSAddr         string
	BindAddress           string
	Cache                 *ttlcache.Cache[string, string]
	ResolveSystem         string
	DoHClient             *doh.Client
	ChunksLengthBeforeSni [2]int
	SniChunksLength       [2]int
	ChunksLengthAfterSni  [2]int
	DelayBetweenChunks    [2]int
}

func (s *Server) getChunkedPackets(data []byte) map[int][]byte {
	chunks := make(map[int][]byte)
	hostname, err := s.getHostname(data)
	fmt.Println("Hostname", string(hostname))
	if err != nil {
		chunks[0] = data
		return chunks
	}
	index := bytes.Index(data, hostname)
	if index == -1 {
		return nil
	}
	// before sni
	chunks[0] = data[0:index]
	// sni
	chunks[1] = data[index : index+len(hostname)]
	// after sni
	chunks[2] = data[index+len(hostname):]
	return chunks
}

// getHostname /* This function is basically all most folks want to invoke out of this
func (s *Server) getHostname(data []byte) ([]byte, error) {
	extensions, err := s.getExtensionBlock(data)
	if err != nil {
		return nil, err
	}
	sn, err := s.getSNBlock(extensions)
	if err != nil {
		return nil, err
	}
	sni, err := s.getSNIBlock(sn)
	if err != nil {
		return nil, err
	}
	return sni, nil
}

/* Return the length computed from the two octets starting at index */
func (s *Server) lengthFromData(data []byte, index int) int {
	if index < 0 || index+1 >= len(data) {
		return 0
	}

	b1 := int(data[index])
	b2 := int(data[index+1])

	return (b1 << 8) + b2
}

// getSNIBlock /* Given a Server Name TLS Extension block, parse out and return the SNI
func (s *Server) getSNIBlock(data []byte) ([]byte, error) {
	index := 0

	for {
		if index >= len(data) {
			break
		}
		length := s.lengthFromData(data, index)
		endIndex := index + 2 + length
		if data[index+2] == 0x00 { /* SNI */
			sni := data[index+3:]
			sniLength := s.lengthFromData(sni, 0)
			return sni[2 : sniLength+2], nil
		}
		index = endIndex
	}
	return []byte{}, fmt.Errorf(
		"finished parsing the SN block without finding an SNI",
	)
}

// getSNBlock /* Given a TLS Extensions data block, go ahead and find the SN block */
func (s *Server) getSNBlock(data []byte) ([]byte, error) {
	index := 0

	if len(data) < 2 {
		return []byte{}, fmt.Errorf("not enough bytes to be an SN block")
	}

	extensionLength := s.lengthFromData(data, index)
	if extensionLength+2 > len(data) {
		return []byte{}, fmt.Errorf("extension looks bonkers")
	}
	data = data[2 : extensionLength+2]

	for {
		if index+4 >= len(data) {
			break
		}
		length := s.lengthFromData(data, index+2)
		endIndex := index + 4 + length
		if data[index] == 0x00 && data[index+1] == 0x00 {
			return data[index+4 : endIndex], nil
		}

		index = endIndex
	}

	return []byte{}, fmt.Errorf(
		"finished parsing the Extension block without finding an SN block",
	)
}

// getExtensionBlock /* Given a raw TLS Client Hello, go ahead and find all the Extensions */
func (s *Server) getExtensionBlock(data []byte) ([]byte, error) {
	/*   data[0]           - content type
	 *   data[1], data[2]  - major/minor version
	 *   data[3], data[4]  - total length
	 *   data[...38+5]     - start of SessionID (length bit)
	 *   data[38+5]        - length of SessionID
	 */
	dataLen := len(data)
	index := s.TLSHeaderLength + 38

	if dataLen <= index+1 {
		return nil, fmt.Errorf("not enough bits to be a Client Hello")
	}

	sessionIDLength := int(data[index])
	newIndex := index + 1 + sessionIDLength
	/* Index is at SessionID Length bit */
	if newIndex+2 >= dataLen {
		return nil, fmt.Errorf("not enough bytes for the SessionID")
	}
	index = newIndex

	cipherListLength := s.lengthFromData(data, index)
	newIndex = index + 2 + cipherListLength
	if newIndex+1 >= dataLen {
		return nil, fmt.Errorf("not enough bytes for the Cipher List")
	}
	index = newIndex

	compressionLength := int(data[index])
	newIndex = index + 1 + compressionLength
	if newIndex >= dataLen {
		return nil, fmt.Errorf("not enough bytes for the compression length")
	}
	index = newIndex
	/* Now we're at the Extension start */
	if len(data[index:]) == 0 {
		return nil, fmt.Errorf("no extensions")
	}

	return data[index:], nil
}
func (s *Server) c(dst io.Writer, src io.Reader, split bool) {
	buf := make([]byte, 32*1024)
	for index := 0; ; index++ {
		nr, er := src.Read(buf)
		if nr > 0 {
			var nw, ew = 0, error(nil)
			if index == 0 && split {
				chunks := s.getChunkedPackets(buf[0:nr])
				for i, chunk := range chunks {
					// if its before sni
					lengthMin, lengthMax := 0, 0
					if i == 0 {
						lengthMin, lengthMax = s.ChunksLengthBeforeSni[0], s.ChunksLengthBeforeSni[1]
					} else if i == 1 { // if its sni
						lengthMin, lengthMax = s.SniChunksLength[0], s.SniChunksLength[1]
					} else { // if its after sni
						lengthMin, lengthMax = s.ChunksLengthAfterSni[0], s.ChunksLengthAfterSni[1]
					}
					position := 0
					for {
						length := rand.Intn(lengthMax-lengthMin) + lengthMin
						delay := rand.Intn(s.DelayBetweenChunks[1]-s.DelayBetweenChunks[0]) + s.DelayBetweenChunks[0]
						ppl := position + length
						if ppl > len(chunk) {
							ppl = len(chunk)
						}
						tnw, ew := dst.Write(chunk[position:ppl])
						if ew != nil {
							return
						}
						nw += tnw
						position = ppl
						if position == len(chunk) {
							break
						}
						time.Sleep(time.Duration(delay) * time.Millisecond)
					}
				}
			} else {
				nw, ew = dst.Write(buf[0:nr])
			}
			if nw < 0 || nr < nw {
				nw = 0
				if ew == nil {
					return
				}
			}
			if ew != nil {
				return
			}
			if nr != nw {
				return
			}
		}
		if er != nil {
			return
		}
	}
}

func (s *Server) Handle(socksCtx context.Context, writer io.Writer, socksRequest *socks5.Request) error {
	// get , dohClient *doh.Client from context
	dohClient := s.DoHClient
	fmt.Println(socksRequest.RawDestAddr)
	dialDest := socksRequest.RawDestAddr.String()
	closeSignal := make(chan error)
	dest := socksRequest.RawDestAddr
	if dest.FQDN != "" {
		ip, err := s.resolve(dest.FQDN, dohClient)
		if err != nil {
			fmt.Printf("resolve error, %v\n", err)
			if err := socks5.SendReply(writer, statute.RepHostUnreachable, nil); err != nil {
				return fmt.Errorf("failed to send reply, %v", err)
			}
			return err
		} else {
			dest.IP = net.ParseIP(ip)
			dialDest = ip + ":" + strconv.Itoa(dest.Port)
		}
	} else {
		fmt.Println("no need to resolve", socksRequest.RawDestAddr)
	}
	if err := socks5.SendReply(writer, statute.RepSuccess, nil); err != nil {
		fmt.Printf("failed to send reply, %v\n", err)
		return fmt.Errorf("failed to send reply, %v", err)
	}
	fmt.Println(dialDest)
	rAddr, err := net.ResolveTCPAddr("tcp", dialDest)
	if err != nil {
		panic(err)
	}

	rConn, err := net.DialTCP("tcp", nil, rAddr)
	if err != nil {
		fmt.Println("unable to connect to", dialDest)
		return err
	}
	err = rConn.SetNoDelay(true)
	if err != nil {
		return err
	}
	defer rConn.Close()
	go s.c(writer, rConn, false)
	s.c(rConn, socksRequest.Reader, true)
	// terminate the connection
	return <-closeSignal
}

func (s *Server) resolve(fqdn string, dohClient *doh.Client) (string, error) {
	if strings.LastIndex(fqdn, ".") != len(fqdn)-1 {
		fqdn += "."
	}
	if s.Cache.Get(fqdn) != nil {
		fmt.Println("use dns cache")
		return s.Cache.Get(fqdn).Value(), nil
	}
	// Create a DNS request
	req := dns.Msg{}
	req.Id = dns.Id()
	req.RecursionDesired = true
	req.Question = []dns.Question{
		{
			Name:   fqdn,
			Qtype:  dns.TypeA,
			Qclass: dns.ClassINET,
		},
	}

	var (
		exchange *dns.Msg
		err      error
	)

	if s.ResolveSystem == "doh" {
		exchange, err = s.resolveThroughDOH(&req, dohClient)
	} else {
		exchange, err = s.resolveThroughSDNS(&req)
	}

	if err != nil {
		fmt.Println(err)
		return "", err
	}

	fmt.Println(exchange.Answer[0])
	record := strings.Fields(exchange.Answer[0].String())
	ttl, err := strconv.Atoi(record[1])
	if err != nil {
		return "", err
	}
	if record[3] == "CNAME" {
		return s.resolve(record[4], dohClient)
	}
	s.Cache.Set(fqdn, record[4], time.Duration(ttl)*time.Second)
	return record[4], nil
}

func (s *Server) resolveThroughDOH(req *dns.Msg, dohClient *doh.Client) (*dns.Msg, error) {
	//exchange, _, err := dohClient.Exchange(req, "https://de-fra.doh.sb/dns-query")
	//exchange, _, err := dohClient.Exchange(req, "https://yarp.lefolgoc.net/dns-query")
	exchange, _, err := dohClient.Exchange(req, s.RemoteDNSAddr)
	if err != nil {
		return nil, err
	}
	if exchange.Answer == nil || len(exchange.Answer) == 0 {
		return nil, fmt.Errorf("no answer")
	}
	return exchange, nil
}

func (s *Server) resolveThroughSDNS(req *dns.Msg) (*dns.Msg, error) {
	// AdGuard DNS stamp
	//stampStr := "sdns://AQcAAAAAAAAAFTEzMy4xMzAuMTE4LjEwMzo1MDQ0MyB7SI0q4_Ff8lFRUCbjPtcAQ3HfdWlLxyGDUUNc3NUZdiIyLmRuc2NyeXB0LWNlcnQuc2FsZG5zMDIudHlwZXEub3Jn"
	//stampStr := "sdns://AQIAAAAAAAAADjM3LjEyMC4xOTMuMjE5IDEzcq1ZVjLCQWuHLwmPhRvduWUoTGy-mk8ZCWQw26laHjIuZG5zY3J5cHQtY2VydC5jcnlwdG9zdG9ybS5pcw"

	// Initializing the DNSCrypt client
	c := dnscrypt.Client{Net: "tcp", Timeout: 10 * time.Second}

	// Fetching and validating the server certificate
	resolverInfo, err := c.Dial(s.RemoteDNSAddr)
	if err != nil {
		return nil, err
	}

	exchange, err := c.Exchange(req, resolverInfo)
	if err != nil {
		return nil, err
	}
	if exchange.Answer == nil || len(exchange.Answer) == 0 {
		return nil, fmt.Errorf("no answer")
	}
	return exchange, nil
}
