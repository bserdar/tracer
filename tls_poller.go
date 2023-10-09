package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strconv"

	"github.com/go-errors/errors"
	"github.com/hashicorp/golang-lru/simplelru"
	"github.com/kubeshark/ebpf/perf"
	"github.com/kubeshark/tracer/misc"
	"github.com/kubeshark/tracer/misc/wcap"
	"github.com/rs/zerolog/log"
)

const (
	fdCachedItemAvgSize = 40
	fdCacheMaxItems     = 500000 / fdCachedItemAvgSize
)

type tlsPoller struct {
	tls            *Tracer
	streams        map[string]*tlsStream
	closeStreams   chan string
	chunksReader   *perf.Reader
	procfs         string
	fdCache        *simplelru.LRU // Actual type is map[string]addressPair
	evictedCounter int
	sorter         *PacketSorter
}

func newTlsPoller(
	tls *Tracer,
	procfs string,
) (*tlsPoller, error) {
	sortedPackets := make(chan *wcap.SortedPacket, misc.PacketChannelBufferSize)
	poller := &tlsPoller{
		tls:          tls,
		streams:      make(map[string]*tlsStream),
		closeStreams: make(chan string, misc.TlsCloseChannelBufferSize),
		chunksReader: nil,
		procfs:       procfs,
		sorter:       NewPacketSorter(sortedPackets),
	}

	fdCache, err := simplelru.NewLRU(fdCacheMaxItems, poller.fdCacheEvictCallback)

	if err != nil {
		return nil, errors.Wrap(err, 0)
	}

	poller.fdCache = fdCache
	return poller, nil
}

func (p *tlsPoller) init(bpfObjects *tracerObjects, bufferSize int) error {
	var err error

	p.chunksReader, err = perf.NewReader(bpfObjects.ChunksBuffer, bufferSize)

	if err != nil {
		return errors.Wrap(err, 0)
	}

	return nil
}

func (p *tlsPoller) close() error {
	return p.chunksReader.Close()
}

func (p *tlsPoller) poll(streamsMap *TcpStreamMap) {
	// tracerTlsChunk is generated by bpf2go.
	chunks := make(chan *tracerTlsChunk)

	go p.pollChunksPerfBuffer(chunks)

	for {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				return
			}

			if err := p.handleTlsChunk(chunk, streamsMap); err != nil {
				LogError(err)
			}
		case key := <-p.closeStreams:
			delete(p.streams, key)
		}
	}
}

func (p *tlsPoller) pollChunksPerfBuffer(chunks chan<- *tracerTlsChunk) {
	log.Info().Msg("Start polling for tls events")

	for {
		record, err := p.chunksReader.Read()

		if err != nil {
			close(chunks)

			if errors.Is(err, perf.ErrClosed) {
				return
			}

			LogError(errors.Errorf("Error reading chunks from tls perf, aborting TLS! %v", err))
			return
		}

		if record.LostSamples != 0 {
			log.Info().Msg(fmt.Sprintf("Buffer is full, dropped %d chunks", record.LostSamples))
			continue
		}

		buffer := bytes.NewReader(record.RawSample)

		var chunk tracerTlsChunk

		if err := binary.Read(buffer, binary.LittleEndian, &chunk); err != nil {
			LogError(errors.Errorf("Error parsing chunk %v", err))
			continue
		}

		chunks <- &chunk
	}
}

func (p *tlsPoller) handleTlsChunk(chunk *tracerTlsChunk, streamsMap *TcpStreamMap) error {
	address := chunk.getAddressPair()

	// Creates one *tlsStream per TCP stream
	key := buildTlsKey(address, chunk.isRequest())
	stream, streamExists := p.streams[key]
	if !streamExists {
		stream = NewTlsStream(p, key)
		stream.setId(streamsMap.NextId())
		streamsMap.Store(stream.getId(), stream)
		p.streams[key] = stream

		stream.client = NewTlsReader(p.buildTcpId(address, true), stream, true)
		stream.server = NewTlsReader(p.buildTcpId(address, false), stream, false)
	}

	reader := chunk.getReader(stream)
	reader.newChunk(chunk)

	return nil
}

func buildTlsKey(address *addressPair, isRequest bool) string {
	if isRequest {
		return fmt.Sprintf("%s:%d>%s:%d", address.srcIp, address.srcPort, address.dstIp, address.dstPort)
	} else {
		return fmt.Sprintf("%s:%d>%s:%d", address.dstIp, address.dstPort, address.srcIp, address.srcPort)
	}
}

func (p *tlsPoller) buildTcpId(address *addressPair, isRequest bool) *TcpID {
	if isRequest {
		return &TcpID{
			SrcIP:   address.srcIp.String(),
			DstIP:   address.dstIp.String(),
			SrcPort: strconv.FormatUint(uint64(address.srcPort), 10),
			DstPort: strconv.FormatUint(uint64(address.dstPort), 10),
		}
	} else {
		return &TcpID{
			SrcIP:   address.dstIp.String(),
			DstIP:   address.srcIp.String(),
			SrcPort: strconv.FormatUint(uint64(address.dstPort), 10),
			DstPort: strconv.FormatUint(uint64(address.srcPort), 10),
		}
	}
}

func (p *tlsPoller) fdCacheEvictCallback(key interface{}, value interface{}) {
	p.evictedCounter = p.evictedCounter + 1

	if p.evictedCounter%1000000 == 0 {
		log.Info().Msg(fmt.Sprintf("Tls fdCache evicted %d items", p.evictedCounter))
	}
}