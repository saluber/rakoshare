package main

import (
	"bytes"
	"crypto/sha1"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zeebo/bencode"

	"github.com/rakoo/rakoshare/pkg/sharesession"
)

var (
	errNewFile    = errors.New("Got new file")
	errInvalidDir = errors.New("Invalid watched dir")
)

type state int

const (
	IDEM = iota
	CHANGED
)

type Watcher struct {
	session    *sharesession.Session
	watchedDir string
	lock       sync.Mutex

	PingNewTorrent chan string
}

func NewWatcher(session *sharesession.Session, watchedDir string) (w *Watcher, err error) {
	w = &Watcher{
		session:        session,
		watchedDir:     watchedDir,
		PingNewTorrent: make(chan string),
	}

	go w.watch()

	// Initialization, only if there is something in the dir
	if _, err := os.Stat(watchedDir); err != nil {
		return nil, err
	}

	st, err := os.Stat(watchedDir)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, errInvalidDir
	}

	go func() {
		w.PingNewTorrent <- session.GetCurrentInfohash()
	}()

	return
}

func (w *Watcher) watch() {
	var previousState, currentState state
	currentState = IDEM

	compareTime := w.session.GetLastModTime()

	// All paths in previous scan, sorted alphabetically
	previousScanPaths := []string{}

	// All paths in current torrent, sorted alphabetically
	torrentPaths := []string{}

	for _ = range time.Tick(10 * time.Second) {
		w.lock.Lock()

		currentTorrent := w.session.GetCurrentTorrent()
		if len(currentTorrent) != 0 {
			torrentPaths = []string{}
			m, err := NewMetaInfoFromContent([]byte(currentTorrent))
			if err == nil {
				for _, f := range m.Info.Files {
					torrentPaths = append(torrentPaths, filepath.Join(f.Path...))
				}
			}
		}

		scanPaths := []string{}

		err := torrentWalk(w.watchedDir, func(path string, info os.FileInfo, perr error) (err error) {
			if info.ModTime().After(compareTime) {
				fmt.Printf("[newer] %s\n", path)
				return errNewFile
			}
			scanPaths = append(scanPaths, path)
			return
		})

		w.lock.Unlock()

		// Check if the folder has changed:
		// - if any number of file was added or removed
		// - if any number of file path were changed
		hasChanged := err == errNewFile
		if len(scanPaths) != len(previousScanPaths) {
			hasChanged = true
		} else {
			for i := range scanPaths {
				if previousScanPaths[i] != scanPaths[i] {
					hasChanged = true
					break
				}
			}
		}

		if hasChanged {
			currentState = CHANGED
		} else if err == nil {
			currentState = IDEM
		} else {
			log.Println("Error while walking dir:", err)
		}

		compareTime = time.Now()

		if currentState == IDEM && previousState == CHANGED {
			// Note that we may be in the CHANGED state for multiple
			// iterations, such as when changes take more than 10 seconds to
			// finish. When we go back to "idle" state, we kick in the
			// metadata creation.

			// Block until we completely manage it. We will take
			// care of other changes in the next run of the loop.
			ih, err := w.torrentify()
			if err != nil {
				log.Printf("Couldn't torrentify: ", err)
				continue
			}
			w.PingNewTorrent <- ih
		}

		previousScanPaths = scanPaths
		previousState = currentState
	}
}

func (w *Watcher) torrentify() (ih string, err error) {
	w.lock.Lock()
	defer w.lock.Unlock()

	meta, err := createMeta(w.watchedDir)
	if err != nil {
		log.Println(err)
		return
	}

	var buf bytes.Buffer
	err = bencode.NewEncoder(&buf).Encode(meta)
	if err != nil {
		return
	}
	w.session.SaveTorrent(buf.Bytes(), meta.InfoHash, time.Now().Format(time.RFC3339))

	return meta.InfoHash, err
}

func createMeta(dir string) (meta *MetaInfo, err error) {
	blockSize := int64(1 << 20) // 1MiB

	fileDicts := make([]*FileDict, 0)

	hasher := NewBlockHasher(blockSize)
	err = torrentWalk(dir, func(path string, info os.FileInfo, perr error) (err error) {
		if perr != nil {
			return perr
		}

		f, err := os.Open(path)
		if err != nil {
			return errors.New(fmt.Sprintf("Couldn't open %s for hashing: %s\n", path, err))
		}
		defer f.Close()

		_, err = io.Copy(hasher, f)
		if err != nil {
			log.Printf("Couldn't hash %s: %s\n", path, err)
			return err
		}

		relPath, err := filepath.Rel(dir, path)
		if err != nil {
			return
		}

		fileDict := &FileDict{
			Length: info.Size(),
			Path:   strings.Split(relPath, string(os.PathSeparator)),
		}
		fileDicts = append(fileDicts, fileDict)

		return
	})
	if err != nil {
		return
	}

	end := hasher.Close()
	if end != nil {
		return
	}

	meta = &MetaInfo{
		Info: &InfoDict{
			Pieces:      string(hasher.Pieces),
			PieceLength: blockSize,
			Private:     0,
			Name:        "rakoshare",
			Files:       fileDicts,
		},
	}

	hash := sha1.New()
	err = bencode.NewEncoder(hash).Encode(meta.Info)
	if err != nil {
		return
	}
	meta.InfoHash = string(hash.Sum(nil))

	return
}

type BlockHasher struct {
	sha1er    hash.Hash
	left      int64
	blockSize int64
	Pieces    []byte
}

func NewBlockHasher(blockSize int64) (h *BlockHasher) {
	return &BlockHasher{
		blockSize: blockSize,
		sha1er:    sha1.New(),
		left:      blockSize,
	}
}

// You shouldn't use this one
func (h *BlockHasher) Write(p []byte) (n int, err error) {
	n2, err := h.ReadFrom(bytes.NewReader(p))
	return int(n2), err
}

func (h *BlockHasher) ReadFrom(rd io.Reader) (n int64, err error) {
	var stop bool

	for {
		if h.left > 0 {
			thisN, err := io.CopyN(h.sha1er, rd, h.left)
			if err != nil {
				if err == io.EOF {
					stop = true
				} else {
					return n, err
				}
			}
			h.left -= thisN
			n += thisN
		}
		if h.left == 0 {
			h.Pieces = h.sha1er.Sum(h.Pieces)
			h.sha1er = sha1.New()
			h.left = h.blockSize
		}

		if stop {
			break
		}
	}

	return
}

func (h *BlockHasher) Close() (err error) {
	if h.left == h.blockSize {
		// We're at the end of a blockSize, we don't have any buffered data
		return
	}
	h.Pieces = h.sha1er.Sum(h.Pieces)
	return
}

func torrentWalk(root string, fn filepath.WalkFunc) (err error) {
	return filepath.Walk(root, func(path string, info os.FileInfo, perr error) (err error) {
		if !info.Mode().IsRegular() {
			return
		}

		// Torrents can't have empty files
		if info.Size() == 0 {
			return
		}

		if strings.HasPrefix(filepath.Base(path), ".") {
			return
		}

		if filepath.Ext(path) == ".part" {
			return
		}

		return fn(path, info, perr)
	})
}
