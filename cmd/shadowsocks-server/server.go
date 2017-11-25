package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"

	ss "github.com/qunxyz/shadowsocks-go/shadowsocks"
	"log"
	"reflect"
	"bytes"
)

const (
	AddrMask        byte = 0xf
	idType  = 0 // address type index
	idIP0   = 1 // ip addres start index
	idDmLen = 1 // domain address length index
	idDm0   = 2 // domain address start index

	typeIPv4 = 1 // type is ipv4 address
	typeDm   = 3 // type is domain address
	typeIPv6 = 4 // type is ipv6 address

	lenIPv4   = net.IPv4len + 2 // ipv4 + 2port
	lenIPv6   = net.IPv6len + 2 // ipv6 + 2port
	lenDmBase = 2               // 1addrLen + 2port, plus addrLen
)

var udp bool
//var leakyBuf *ss.LeakyBufType
var Logger = ss.Logger

func getRequest(conn *ss.Conn) (host string, err error) {
	ss.SetReadTimeout(conn)

	// buf size should at least have the same size with the largest possible
	// request size (when addrType is 3, domain name has at most 256 bytes)
	// 1(addrType) + 1(lenByte) + 255(max length address) + 2(port) + 10(hmac-sha1)
	var n int
	var data []byte
	buf := make([]byte, 2048)
	// read till we get possible domain length field
	if n, err = conn.Read(buf); err != nil {
		Logger.Fields(ss.LogFields{
			"buf": string(buf),
			"err": err,
		}).Warn("Read buffer error")
		return
	}

	cipher := conn.Cipher

	if reflect.TypeOf(cipher).String() == "*shadowsocks.CipherStream" {
		data, err = cipher.(*ss.CipherStream).UnPack(buf[0:n])
	} else if reflect.TypeOf(cipher).String() == "*shadowsocks.CipherAead" {
		data, err = cipher.(*ss.CipherAead).UnPack(buf[0:n])
	}
	if _, err = io.ReadFull(bytes.NewReader(data), data[:idType+1]); err != nil {
		Logger.Fields(ss.LogFields{
			"data": string(data),
			"err": err,
		}).Warn("Read data error")
		return
	}

	var reqEnd int
	addrType := data[idType]
	switch addrType & AddrMask {
	case typeIPv4:
		reqEnd = idIP0+lenIPv4
	case typeIPv6:
		reqEnd = idIP0+lenIPv6
	case typeDm:
		reqEnd = idDm0+int(data[idDmLen])+lenDmBase
	default:
		Logger.Fields(ss.LogFields{
			"data": data,
			"addrType": addrType,
			"AddrMask": AddrMask,
			"addrType&AddrMask": addrType&AddrMask,
		}).Warn("addr type not supported")
		err = fmt.Errorf("addr type %d not supported", addrType&AddrMask)
		return
	}

	// Return string for typeIP is not most efficient, but browsers (Chrome,
	// Safari, Firefox) all seems using typeDm exclusively. So this is not a
	// big problem.
	switch addrType & AddrMask {
	case typeIPv4:
		host = net.IP(data[idIP0 : idIP0+net.IPv4len]).String()
	case typeIPv6:
		host = net.IP(data[idIP0 : idIP0+net.IPv6len]).String()
	case typeDm:
		host = string(data[idDm0 : idDm0+int(data[idDmLen])])
	}
	// parse port
	port := binary.BigEndian.Uint16(data[reqEnd-2 : reqEnd])
	host = net.JoinHostPort(host, strconv.Itoa(int(port)))
	return
}

const logCntDelta = 100

var connCnt int
var nextLogConnCnt = logCntDelta

func handleConnection(conn *ss.Conn) {
	var host string

	connCnt++ // this maybe not accurate, but should be enough
	if connCnt-nextLogConnCnt >= 0 {
		// XXX There's no xadd in the atomic package, so it's difficult to log
		// the message only once with low cost. Also note nextLogConnCnt maybe
		// added twice for current peak connection number level.
		Logger.Infof("Number of client connections reaches %d\n", nextLogConnCnt)
		nextLogConnCnt += logCntDelta
	}

	// function arguments are always evaluated, so surround debug statement
	// with if statement
	Logger.Infof("new client %s->%s\n", conn.RemoteAddr().String(), conn.LocalAddr())
	closed := false
	defer func() {
		Logger.Infof("closed pipe %s<->%s\n", conn.RemoteAddr(), host)
		connCnt--
		if !closed {
			conn.Close()
		}
	}()

	host, err := getRequest(conn)
	if err != nil {
		Logger.Fields(ss.LogFields{
			"RemoteAddr": conn.RemoteAddr(),
			"LocalAddr": conn.LocalAddr(),
			"err": err,
		}).Warn("error getting request")
		closed = true
		return
	}
	// ensure the host does not contain some illegal characters, NUL may panic on Win32
	if strings.ContainsRune(host, 0x00) {
		Logger.Fields(ss.LogFields{"host": host}).Warn("invalid domain name.")
		closed = true
		return
	}
	Logger.Info("connecting ", host)
	remote, err := net.Dial("tcp", host)
	if err != nil {
		if ne, ok := err.(*net.OpError); ok && (ne.Err == syscall.EMFILE || ne.Err == syscall.ENFILE) {
			// log too many open file error
			// EMFILE is process reaches open file limits, ENFILE is system limit
			Logger.Fields(ss.LogFields{
				"host": host,
				"err": err,
			}).Error("dial error")
		} else {
			Logger.Fields(ss.LogFields{
				"host": host,
				"err": err,
			}).Error("error connecting")
		}
		return
	}
	defer func() {
		if !closed {
			remote.Close()
		}
	}()
	Logger.Infof("piping %s<->%s", conn.RemoteAddr(), host)
	go ss.PipeDecrypt(conn, remote, conn.Cipher)
	ss.PipeEncrypt(remote, conn,  conn.Cipher)
	closed = true
	return
}

type PortListener struct {
	password string
	listener net.Listener
}

type UDPListener struct {
	password string
	listener *net.UDPConn
}

type PasswdManager struct {
	sync.Mutex
	portListener map[string]*PortListener
	udpListener  map[string]*UDPListener
}

func (pm *PasswdManager) add(port, password string, listener net.Listener) {
	pm.Lock()
	pm.portListener[port] = &PortListener{password, listener}
	pm.Unlock()
}

func (pm *PasswdManager) addUDP(port, password string, listener *net.UDPConn) {
	pm.Lock()
	pm.udpListener[port] = &UDPListener{password, listener}
	pm.Unlock()
}

func (pm *PasswdManager) get(port string) (pl *PortListener, ok bool) {
	pm.Lock()
	pl, ok = pm.portListener[port]
	pm.Unlock()
	return
}

func (pm *PasswdManager) getUDP(port string) (pl *UDPListener, ok bool) {
	pm.Lock()
	pl, ok = pm.udpListener[port]
	pm.Unlock()
	return
}

func (pm *PasswdManager) del(port string) {
	pl, ok := pm.get(port)
	if !ok {
		return
	}
	if udp {
		upl, ok := pm.getUDP(port)
		if !ok {
			return
		}
		upl.listener.Close()
	}
	pl.listener.Close()
	pm.Lock()
	delete(pm.portListener, port)
	if udp {
		delete(pm.udpListener, port)
	}
	pm.Unlock()
}

// Update port password would first close a port and restart listening on that
// port. A different approach would be directly change the password used by
// that port, but that requires **sharing** password between the port listener
// and password manager.
func (pm *PasswdManager) updatePortPasswd(port, password string) {
	pl, ok := pm.get(port)
	if !ok {
		Logger.Fields(ss.LogFields{"port": port}).Warn("new port added")
	} else {
		if pl.password == password {
			return
		}
		Logger.Warnf("closing port %s to update password\n", port)
		pl.listener.Close()
	}
	// run will add the new port listener to passwdManager.
	// So there maybe concurrent access to passwdManager and we need lock to protect it.
	go run(port, password)
	//if udp {
	//	pl, _ := pm.getUDP(port)
	//	pl.listener.Close()
	//	go runUDP(port, password)
	//}
}

var passwdManager = PasswdManager{portListener: map[string]*PortListener{}, udpListener: map[string]*UDPListener{}}

func updatePasswd() {
	Logger.Info("updating password")
	newconfig, err := ss.ParseConfig(configFile)
	if err != nil {
		Logger.Fields(ss.LogFields{
			"configFile": configFile,
			"err": err,
		}).Error("error parsing config file to update password")
		return
	}
	oldconfig := config
	config = newconfig

	if err = unifyPortPassword(config); err != nil {
		return
	}
	for port, passwd := range config.PortPassword {
		passwdManager.updatePortPasswd(port, passwd)
		if oldconfig.PortPassword != nil {
			delete(oldconfig.PortPassword, port)
		}
	}
	// port password still left in the old config should be closed
	for port := range oldconfig.PortPassword {
		Logger.Infof("closing port %s as it's deleted\n", port)
		passwdManager.del(port)
	}
	Logger.Info("password updated")
}

func waitSignal() {
	var sigChan = make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGHUP)
	for sig := range sigChan {
		if sig == syscall.SIGHUP {
			updatePasswd()
		} else {
			// is this going to happen?
			Logger.Warnf("caught signal %v, exit", sig)
			os.Exit(0)
		}
	}
}

func run(port, password string) {
	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		Logger.Fields(ss.LogFields{
			"port": port,
			"err": err,
		}).Error("error listening port")
		os.Exit(1)
	}
	passwdManager.add(port, password, ln)
	var cipher interface{}
	Logger.Fields(ss.LogFields{"port": port}).Info("server listening ...")
	for {
		conn, err := ln.Accept()
		if err != nil {
			Logger.Fields(ss.LogFields{"err": err}).Error("accept error")
			// listener maybe closed to update password
			return
		}
		// Creating cipher upon first connection.
		if cipher == nil {
			Logger.Info("creating cipher for port:", port)
			cipher, err = ss.NewCipher(config.Method, password)
			if err != nil {
				Logger.Fields(ss.LogFields{
					"port": port,
					"err": err,
				}).Error("Error generating cipher for port")
				conn.Close()
				continue
			}
		}
		go handleConnection(ss.NewConn(conn, ss.CopyCipher(cipher)))
	}
}

//func runUDP(port, password string) {
//	var cipher *ss.Cipher
//	port_i, _ := strconv.Atoi(port)
//	log.Printf("listening udp port %v\n", port)
//	conn, err := net.ListenUDP("udp", &net.UDPAddr{
//		IP:   net.IPv6zero,
//		Port: port_i,
//	})
//	passwdManager.addUDP(port, password, conn)
//	if err != nil {
//		log.Printf("error listening udp port %v: %v\n", port, err)
//		return
//	}
//	defer conn.Close()
//	cipher, err = ss.NewCipher(config.Method, password)
//	if err != nil {
//		log.Printf("Error generating cipher for udp port: %s %v\n", port, err)
//		conn.Close()
//	}
//	SecurePacketConn := ss.NewSecurePacketConn(conn, cipher.Copy())
//	for {
//		if err := ss.ReadAndHandleUDPReq(SecurePacketConn); err != nil {
//			debug.Println(err)
//		}
//	}
//}

func enoughOptions(config *ss.Config) bool {
	return config.ServerPort != 0 && config.Password != ""
}

func unifyPortPassword(config *ss.Config) (err error) {
	if len(config.PortPassword) == 0 { // this handles both nil PortPassword and empty one
		if !enoughOptions(config) {
			ss.Logger.Fatal("must specify both port and password")
			return errors.New("not enough options")
		}
		port := strconv.Itoa(config.ServerPort)
		config.PortPassword = map[string]string{port: config.Password}
	} else {
		if config.Password != "" || config.ServerPort != 0 {
			ss.Logger.Fatal("given port_password, ignore server_port and password option")
		}
	}
	return
}

var configFile string
var config *ss.Config

func main() {
	log.SetOutput(os.Stdout)

	var cmdConfig ss.Config
	var printVer bool
	var core int

	flag.BoolVar(&printVer, "version", false, "print version")
	flag.StringVar(&configFile, "c", "config.json", "specify config file")
	flag.StringVar(&cmdConfig.Password, "k", "", "password")
	flag.IntVar(&cmdConfig.ServerPort, "p", 0, "server port")
	flag.IntVar(&cmdConfig.Timeout, "t", 300, "timeout in seconds")
	flag.StringVar(&cmdConfig.Method, "m", "", "encryption method, default: aes-256-cfb")
	flag.IntVar(&core, "core", 0, "maximum number of CPU cores to use, default is determinied by Go runtime")
	flag.BoolVar((*bool)(&ss.DebugLog), "d", false, "print debug message")
	flag.BoolVar(&udp, "u", false, "UDP Relay")
	flag.Parse()

	if printVer {
		ss.PrintVersion()
		os.Exit(0)
	}

	var err error
	config, err = ss.ParseConfig(configFile)
	if err != nil {
		ss.Logger.Errorf("error reading %s: %v\n", configFile, err)
		config = &cmdConfig
		ss.UpdateConfig(config, config)
	} else {
		ss.UpdateConfig(config, &cmdConfig)
	}
	if config.Method == "" {
		config.Method = "aes-256-cfb"
		ss.Logger.Warn("use aes-256-cfb method, cause not specify method")
	}
	if err = ss.CheckCipherMethod(config.Method); err != nil {
		fmt.Fprintln(os.Stderr, err)
		ss.Logger.Fields(ss.LogFields{
			"method": config.Method,
			"err": err,
		}).Error("check cipher method error")
		os.Exit(1)
	}
	if err = unifyPortPassword(config); err != nil {
		os.Exit(1)
	}
	if core > 0 {
		runtime.GOMAXPROCS(core)
	}
	for port, password := range config.PortPassword {
		go run(port, password)
		//if udp {
		//	go runUDP(port, password)
		//}
	}

	waitSignal()
}
