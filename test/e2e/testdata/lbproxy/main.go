// Command lbproxy stands in for a load balancer in TCP mode (HAProxy's
// "mode tcp", for example) in the e2e suite. For each connection it accepts,
// it dials the backend: when the backend answers it relays both ways, and when
// it does not, it closes the client's connection, as such a load balancer does
// in front of a control plane that does not exist yet. The suite builds it
// statically and runs it inside a node container.
package main

import (
	"flag"
	"io"
	"log"
	"net"
	"os"
	"time"
)

func main() {
	listen := flag.String("listen", "", "host:port to accept on")
	backend := flag.String("backend", "", "host:port to relay to")
	flag.Parse()
	if *listen == "" || *backend == "" {
		flag.Usage()
		os.Exit(2)
	}
	log.SetFlags(0)
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("lbproxy: %v", err)
	}
	log.Printf("lbproxy: listening on %s, backend %s", *listen, *backend)
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Fatalf("lbproxy: %v", err)
		}
		go relay(c, *backend)
	}
}

func relay(c net.Conn, backend string) {
	defer func() { _ = c.Close() }()
	b, err := net.DialTimeout("tcp", backend, 2*time.Second)
	if err != nil {
		log.Printf("lbproxy: no backend for %s (%v), closing", c.RemoteAddr(), err)
		return
	}
	defer func() { _ = b.Close() }()
	log.Printf("lbproxy: relaying %s", c.RemoteAddr())
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(b, c); _ = b.(*net.TCPConn).CloseWrite(); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, b); _ = c.(*net.TCPConn).CloseWrite(); done <- struct{}{} }()
	<-done
	<-done
}
