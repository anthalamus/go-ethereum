package bzz

import (
	"errors"
	"sync"
	// "time"
	"fmt"

	ethlogger "github.com/ethereum/go-ethereum/logger"
	// "github.com/ethereum/go-ethereum/rlp"
)

/*
DPA provides the client API entrypoints Store and Retrieve to store and retrieve
It can store anything that has a byte slice representation, so files or serialised objects etc.

Storage: DPA calls the Chunker to segment the input datastream of any size to a merkle hashed tree of chunks. The key of the root block is returned to the client.

Retrieval: given the key of the root block, the DPA retrieves the block chunks and reconstructs the original data and passes it back as a lazy reader. A lazy reader is a reader with on-demand delayed processing, i.e. the chunks needed to reconstruct a large file are only fetched and processed if that particular part of the document is actually read.

As the chunker produces chunks, DPA dispatches them to the chunk store for storage or retrieval.

The ChunkStore interface is implemented by :

- memStore: a memory cache
- dbStore: local disk/db store
- localStore: a combination (sequence of) memStoe and dbStoe
- netStore: dht storage
*/

const (
	storeChanCapacity    = 100
	retrieveChanCapacity = 100
)

var (
	notFound = errors.New("not found")
)

var dpaLogger = ethlogger.NewLogger("BZZ")

type DPA struct {
	Chunker    Chunker
	ChunkStore ChunkStore
	storeC     chan *Chunk
	retrieveC  chan *Chunk

	lock    sync.Mutex
	running bool
	wg      sync.WaitGroup
	quitC   chan bool
}

// Chunk serves also serves as a request object passed to ChunkStores
// in case it is a retrieval request, Data is nil and Size is 0
// Note that Size is not the size of the data chunk, which is Data.Size() see SectionReader
// but the size of the subtree encoded in the chunk
// 0 if request, to be supplied by the dpa
type Chunk struct {
	Reader SectionReader  // nil if request, to be supplied by dpa
	Data   []byte         // nil if request, to be supplied by dpa
	Size   int64          // size of the data covered by the subtree encoded in this chunk
	Key    Key            // always
	C      chan bool      // to signal data delivery by the dpa
	req    *requestStatus //
}

type ChunkStore interface {
	Put(*Chunk) // effectively there is no error even if there is no error
	Get(Key) (*Chunk, error)
}

func (self *DPA) Retrieve(key Key) (data LazySectionReader) {

	reader, errC := self.Chunker.Join(key, self.retrieveC)
	data = reader
	// we can add subscriptions etc. or timeout here
	go func() {
	LOOP:
		for {
			select {
			case err, ok := <-errC:
				if err != nil {
					dpaLogger.Warnf("%v", err)
				}
				if !ok {
					break LOOP
				}
			case <-self.quitC:
				return
			}
		}
	}()

	return
}

func (self *DPA) Store(data SectionReader) (key Key, err error) {

	errC := self.Chunker.Split(key, data, self.storeC)

	go func() {
	LOOP:
		for {
			select {
			case err, ok := <-errC:
				dpaLogger.Warnf("%v", err)
				if !ok {
					break LOOP
				}

			case <-self.quitC:
				break LOOP
			}
		}
	}()
	return

}

func (self *DPA) Start() {
	self.lock.Lock()
	defer self.lock.Unlock()
	if self.running {
		return
	}
	self.running = true
	self.quitC = make(chan bool)
	self.storeLoop()
	self.retrieveLoop()
}

func (self *DPA) Stop() {
	self.lock.Lock()
	defer self.lock.Unlock()
	if !self.running {
		return
	}
	self.running = false
	close(self.quitC)
}

func (self *DPA) retrieveLoop() {
	self.retrieveC = make(chan *Chunk, retrieveChanCapacity)

	go func() {
	RETRIEVE:
		for chunk := range self.retrieveC {
			go func() {
				storedChunk, err := self.ChunkStore.Get(chunk.Key)
				if err == notFound {
					dpaLogger.DebugDetailf("chunk %x not found", chunk.Key)
					return
				}
				if err != nil {
					dpaLogger.DebugDetailf("error retrieving chunk %x: %v", chunk.Key, err)
					return
				}
				chunk.Reader = NewChunkReaderFromBytes(storedChunk.Data)
				chunk.Size = storedChunk.Size
				close(chunk.C)
			}()
			select {
			case <-self.quitC:
				break RETRIEVE
			default:
			}
		}
	}()
}

func (self *DPA) storeLoop() {
	self.storeC = make(chan *Chunk)
	go func() {
		fmt.Printf("StoreLoop started.\n")
	STORE:
		for {
			chunk := <-self.storeC
			fmt.Printf("StoreLoop reader size %d\n", chunk.Reader.Size())
			chunk.Data = make([]byte, chunk.Reader.Size())
			chunk.Reader.ReadAt(chunk.Data, 0)
			self.ChunkStore.Put(chunk)
			select {
			case <-self.quitC:
				break STORE
			default:
			}
		}
	}()
}
