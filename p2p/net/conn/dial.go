package conn

import (
	"fmt"
	"math/rand"
	"strings"

	addrutil "github.com/ipfs/go-libp2p/p2p/net/swarm/addr"
	transport "gx/ipfs/QmR4HhZm1fL8epXz8661Ru8ks5Y1Uk2u51quAWaCTD1zHg/go-libp2p-transport"
	lgbl "gx/ipfs/QmSyBhZt2upyQ3NJmTpab1pX6hesA59vcYTGmgoDorZZbw/go-libp2p-loggables"
	msmux "gx/ipfs/QmUeEcYJrzAEKdQXjzTxCgNZgc9sRuwharsvzzm5Gd2oGB/go-multistream"
	peer "gx/ipfs/QmY1xNhBfF9xA1pmD8yejyQAyd77K68qNN6JPM1CN2eiRu/go-libp2p-peer"
	manet "gx/ipfs/QmYVqhVfbK4BKvbW88Lhm26b3ud14sTBvcm1H7uWUx1Fkp/go-multiaddr-net"
	context "gx/ipfs/QmZy2y8t9zQH2a1b8q2ZSLKp17ATuJoCNxxyMFG5qFExpt/go-net/context"
	ci "gx/ipfs/QmaP38GJApheTr84f8R89vsT7oJLQw1AeCz4HqrQgv2njB/go-libp2p-crypto"
	ma "gx/ipfs/QmcobAGsCjYt5DXoq9et9L8yR8er7o7Cu3DTvpaq12jYSz/go-multiaddr"
)

type WrapFunc func(transport.Conn) transport.Conn

func NewDialer(p peer.ID, pk ci.PrivKey, wrap WrapFunc) *Dialer {
	return &Dialer{
		LocalPeer:  p,
		PrivateKey: pk,
		Wrapper:    wrap,
		fallback:   new(transport.FallbackDialer),
	}
}

// String returns the string rep of d.
func (d *Dialer) String() string {
	return fmt.Sprintf("<Dialer %s ...>", d.LocalPeer)
}

// Dial connects to a peer over a particular address
// Ensures raddr is part of peer.Addresses()
// Example: d.DialAddr(ctx, peer.Addresses()[0], peer)
func (d *Dialer) Dial(ctx context.Context, raddr ma.Multiaddr, remote peer.ID) (Conn, error) {
	logdial := lgbl.Dial("conn", d.LocalPeer, remote, nil, raddr)
	logdial["encrypted"] = (d.PrivateKey != nil) // log wether this will be an encrypted dial or not.
	defer log.EventBegin(ctx, "connDial", logdial).Done()

	var connOut Conn
	var errOut error
	done := make(chan struct{})

	// do it async to ensure we respect don contexteone
	go func() {
		defer func() {
			select {
			case done <- struct{}{}:
			case <-ctx.Done():
			}
		}()

		maconn, err := d.rawConnDial(ctx, raddr, remote)
		if err != nil {
			errOut = err
			return
		}

		if d.Wrapper != nil {
			maconn = d.Wrapper(maconn)
		}

		cryptoProtoChoice := SecioTag
		if !EncryptConnections {
			cryptoProtoChoice = NoEncryptionTag
		}

		err = msmux.SelectProtoOrFail(cryptoProtoChoice, maconn)
		if err != nil {
			errOut = err
			return
		}

		c, err := newSingleConn(ctx, d.LocalPeer, remote, maconn)
		if err != nil {
			maconn.Close()
			errOut = err
			return
		}

		if d.PrivateKey == nil || EncryptConnections == false {
			log.Warning("dialer %s dialing INSECURELY %s at %s!", d, remote, raddr)
			connOut = c
			return
		}

		c2, err := newSecureConn(ctx, d.PrivateKey, c)
		if err != nil {
			errOut = err
			c.Close()
			return
		}

		connOut = c2
	}()

	select {
	case <-ctx.Done():
		logdial["error"] = ctx.Err()
		logdial["dial"] = "failure"
		return nil, ctx.Err()
	case <-done:
		// whew, finished.
	}

	if errOut != nil {
		logdial["error"] = errOut
		logdial["dial"] = "failure"
		return nil, errOut
	}

	logdial["dial"] = "success"
	return connOut, nil
}

func (d *Dialer) AddDialer(pd transport.Dialer) {
	d.Dialers = append(d.Dialers, pd)
}

// returns dialer that can dial the given address
func (d *Dialer) subDialerForAddr(raddr ma.Multiaddr) transport.Dialer {
	for _, pd := range d.Dialers {
		if pd.Matches(raddr) {
			return pd
		}
	}

	if d.fallback.Matches(raddr) {
		return d.fallback
	}

	return nil
}

// rawConnDial dials the underlying net.Conn + manet.Conns
func (d *Dialer) rawConnDial(ctx context.Context, raddr ma.Multiaddr, remote peer.ID) (transport.Conn, error) {
	if strings.HasPrefix(raddr.String(), "/ip4/0.0.0.0") {
		log.Event(ctx, "connDialZeroAddr", lgbl.Dial("conn", d.LocalPeer, remote, nil, raddr))
		return nil, fmt.Errorf("Attempted to connect to zero address: %s", raddr)
	}

	sd := d.subDialerForAddr(raddr)
	if sd == nil {
		return nil, fmt.Errorf("no dialer for %s", raddr)
	}

	return sd.Dial(raddr)
}

func pickLocalAddr(laddrs []ma.Multiaddr, raddr ma.Multiaddr) (laddr ma.Multiaddr) {
	if len(laddrs) < 1 {
		return nil
	}

	// make sure that we ONLY use local addrs that match the remote addr.
	laddrs = manet.AddrMatch(raddr, laddrs)
	if len(laddrs) < 1 {
		return nil
	}

	// make sure that we ONLY use local addrs that CAN dial the remote addr.
	// filter out all the local addrs that aren't capable
	raddrIPLayer := ma.Split(raddr)[0]
	raddrIsLoopback := manet.IsIPLoopback(raddrIPLayer)
	raddrIsLinkLocal := manet.IsIP6LinkLocal(raddrIPLayer)
	laddrs = addrutil.FilterAddrs(laddrs, func(a ma.Multiaddr) bool {
		laddrIPLayer := ma.Split(a)[0]
		laddrIsLoopback := manet.IsIPLoopback(laddrIPLayer)
		laddrIsLinkLocal := manet.IsIP6LinkLocal(laddrIPLayer)
		if laddrIsLoopback { // our loopback addrs can only dial loopbacks.
			return raddrIsLoopback
		}
		if laddrIsLinkLocal {
			return raddrIsLinkLocal // out linklocal addrs can only dial link locals.
		}
		return true
	})

	// TODO pick with a good heuristic
	// we use a random one for now to prevent bad addresses from making nodes unreachable
	// with a random selection, multiple tries may work.
	return laddrs[rand.Intn(len(laddrs))]
}

// MultiaddrProtocolsMatch returns whether two multiaddrs match in protocol stacks.
func MultiaddrProtocolsMatch(a, b ma.Multiaddr) bool {
	ap := a.Protocols()
	bp := b.Protocols()

	if len(ap) != len(bp) {
		return false
	}

	for i, api := range ap {
		if api.Code != bp[i].Code {
			return false
		}
	}

	return true
}

// MultiaddrNetMatch returns the first Multiaddr found to match  network.
func MultiaddrNetMatch(tgt ma.Multiaddr, srcs []ma.Multiaddr) ma.Multiaddr {
	for _, a := range srcs {
		if MultiaddrProtocolsMatch(tgt, a) {
			return a
		}
	}
	return nil
}