package sniffer

import (
	"errors"
	"io"
	"os"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/codahale/metrics"
	"github.com/emfree/gopacket"
	"github.com/emfree/gopacket/afpacket"
	"github.com/emfree/gopacket/layers"
	"github.com/emfree/gopacket/pcap"
	"github.com/emfree/gopacket/reassembly"
)

const (
	PCap     = "pcap"
	Afpacket = "af_packet"
	Offline  = "offline"
)

type Options struct {
	SourceType   string `long:"type" default:"pcap" description:"Packet capture mechanism (pcap, af_packet or offline)"`
	Device       string `long:"device" description:"Network interface to listen on"`
	SnapLen      int    `long:"snaplen" default:"65535" description:"Capture snapshot length"`
	BufSizeMb    int    `long:"bufsize" description:"AF_PACKET buffer size in megabytes" default:"30"`
	FlushTimeout int    `long:"flushtimeout" description:"Time in seconds to wait before flushing buffered data for a connection" default:"60"`
	PcapFile     string `long:"pcapfile" description:"For offline packet captures, path to pcap file"`
}

type Sniffer struct {
	packetSource    packetDataSource
	consumerFactory ConsumerFactory
	flushTimeout    time.Duration
	closeTimeout    time.Duration
}

type packetDataSource interface {
	gopacket.PacketDataSource
	SetBPFFilter(filter string) error
	LinkLayerType() gopacket.LayerType
}

type afpacketSource struct {
	*afpacket.TPacket
}

func (a *afpacketSource) LinkLayerType() gopacket.LayerType {
	return layers.LayerTypeEthernet
}

type pcapSource struct {
	*pcap.Handle
}

func (p *pcapSource) LinkLayerType() gopacket.LayerType {
	if p.LinkType() == layers.LinkTypeLinuxSLL {
		return layers.LayerTypeLinuxSLL
	} else {
		return layers.LayerTypeEthernet
	}
}

func New(options Options, cf ConsumerFactory) (*Sniffer, error) {
	flushTimeout := time.Duration(options.FlushTimeout) * time.Second
	// TODO: does the close timeout really need to be configurable?
	closeTimeout := time.Duration(3600) * time.Second
	s := &Sniffer{
		consumerFactory: cf,
		flushTimeout:    flushTimeout,
		closeTimeout:    closeTimeout,
	}
	var err error
	if options.SourceType == PCap {
		s.packetSource, err = newPcapHandle(options.Device, options.SnapLen, pcap.BlockForever)
		if err != nil {
			return nil, err
		}
	} else if options.SourceType == Afpacket {
		frameSize, blockSize, numBlocks, err := afpacketComputeSize(options.BufSizeMb, options.SnapLen, os.Getpagesize())
		if err != nil {
			return nil, err
		}
		s.packetSource, err = newAfpacketHandle(options.Device, frameSize, blockSize, numBlocks, pcap.BlockForever)
		if err != nil {
			return nil, err
		}
	} else if options.SourceType == Offline {
		s.packetSource, err = newOfflineHandle(options.PcapFile)
		if err != nil {
			return nil, err
		}
	} else {
		return nil, errors.New("Unsupported packet source type")
	}

	// TODO: support concatenating multiple BPF filters
	err = s.packetSource.SetBPFFilter(cf.BPFFilter())
	if err != nil {
		return nil, err
	}

	return s, nil

}

func (sniffer *Sniffer) Run() error {
	factory := NewStreamFactory(sniffer.consumerFactory)
	streamPool := reassembly.NewStreamPool(factory)
	assembler := reassembly.NewAssembler(streamPool)

	linkLayerType := sniffer.packetSource.LinkLayerType()
	var sll layers.LinuxSLL
	var eth layers.Ethernet
	var ip4 layers.IPv4
	var ip6 layers.IPv6
	var tcp layers.TCP
	var payload gopacket.Payload
	decoder := gopacket.NewDecodingLayerParser(linkLayerType, &sll, &eth, &ip4, &ip6, &tcp, &payload)
	decodedLayers := make([]gopacket.LayerType, 0, 10)
	parsedPackets := metrics.Counter("sniffer.parsed_packets")
	unparseablePackets := metrics.Counter("sniffer.unparseable_packets")
	ctr := 0

loop:
	for {
		packetData, ci, err := sniffer.packetSource.ReadPacketData()

		if err == io.EOF {
			logrus.Info("EOF") // debug -- better handle this
			break
		} else if err != nil {
			logrus.WithError(err).Info("Error reading packet")
			continue
		}

		if len(packetData) == 0 {
			continue
		}

		err = decoder.DecodeLayers(packetData, &decodedLayers)

		if err != nil {
			logrus.WithError(err).Error("Error decoding packet")
			continue
		}

		ctr++
		flushOptions := reassembly.FlushOptions{
			T:  ci.Timestamp.Add(-sniffer.flushTimeout),
			TC: ci.Timestamp.Add(-sniffer.closeTimeout),
		}
		if ctr%1000 == 0 {
			assembler.FlushWithOptions(flushOptions)
		}

		var netFlow gopacket.Flow
		foundNetLayer := false

		for _, typ := range decodedLayers {
			switch typ {
			case layers.LayerTypeIPv4:
				netFlow = ip4.NetworkFlow()
				foundNetLayer = true
			case layers.LayerTypeIPv6:
				netFlow = ip6.NetworkFlow()
				foundNetLayer = true
			case layers.LayerTypeTCP:
				if foundNetLayer {
					assembler.AssembleWithContext(netFlow, &tcp, &Context{CaptureInfo: ci})
					parsedPackets.Add()
					continue loop
				}
			}
		}
		logrus.WithFields(logrus.Fields{"decodedLayers": decodedLayers}).Debug("Couldn't decode packet, ignoring")
		unparseablePackets.Add()
	}
	return nil
}

func newPcapHandle(iface string, snaplen int, pollTimeout time.Duration) (*pcapSource, error) {
	if iface == "" {
		iface = "any"
	}
	h, err := pcap.OpenLive(iface, int32(snaplen), true, pollTimeout)
	return &pcapSource{h}, err
}

func newAfpacketHandle(iface string, frameSize int, blockSize int, numBlocks int,
	pollTimeout time.Duration) (*afpacketSource, error) {
	h, err := afpacket.NewTPacket(
		afpacket.OptInterface(iface),
		afpacket.OptFrameSize(frameSize),
		afpacket.OptBlockSize(blockSize),
		afpacket.OptNumBlocks(numBlocks),
		afpacket.OptPollTimeout(pollTimeout),
		afpacket.OptTPacketVersion(afpacket.TPacketVersion2)) // work around https://github.com/google/gopacket/issues/80
	return &afpacketSource{h}, err
}

func newOfflineHandle(filepath string) (*pcapSource, error) {
	h, err := pcap.OpenOffline(filepath)
	return &pcapSource{h}, err
}

func afpacketComputeSize(targetSizeMb int, snaplen int, pageSize int) (
	frameSize int, blockSize int, numBlocks int, err error) {
	if snaplen < pageSize {
		frameSize = pageSize / (pageSize / snaplen)
	} else {
		frameSize = (snaplen/pageSize + 1) * pageSize
	}
	blockSize = frameSize * afpacket.DefaultNumBlocks
	numBlocks = (targetSizeMb * 1024 * 1024) / blockSize
	if numBlocks == 0 {
		return 0, 0, 0, errors.New("Buffer size too small")
	}

	return frameSize, blockSize, numBlocks, nil
}
