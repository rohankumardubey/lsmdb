package db

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/pierrec/lz4"
)

type MEMSSTable struct {
	activeTable *SSTable
	immutable   []*SSTable
	sparseIndex []*SparseIndex
	wal         *wal

	lock          sync.RWMutex
	id            uint64
	rootPath      string
	blockKeyNum   uint16
	tableBlockNum uint16
}

func NewMEMSSTable(rootPath string, blockKeyNum, tableBlockNum uint16) (*MEMSSTable, error) {
	t := new(MEMSSTable)
	t.rootPath = rootPath
	t.blockKeyNum = blockKeyNum
	t.tableBlockNum = tableBlockNum
	t.activeTable = NewSSTable()
	var err error
	if err = os.MkdirAll(t.rootPath, 0755); err != nil {
		return nil, err
	}
	t.wal, err = NewWAL(fmt.Sprintf("%s/%d.wal", t.rootPath, t.id))
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (t *MEMSSTable) Set(key, val string) error {
	return t.command(&Command{Key: key, Value: val, Command: CommandTypeSet}, false)
}

func (t *MEMSSTable) Delete(key string) error {
	return t.command(&Command{Key: key, Command: CommandTypeDelete}, false)
}

func (t *MEMSSTable) command(c *Command, restore bool) error {
	t.lock.Lock()
	if t.activeTable.Len() >= int(t.blockKeyNum) {
		t.switchTable()
	}
	if !restore {
		t.wal.Append(c)
	}
	t.activeTable.Append(c)
	t.lock.Unlock()
	return nil
}

func (t *MEMSSTable) Query(key string) (string, error) {
	// first lookup activity table
	if v := t.activeTable.Query(key); v != nil {
		return v.Value, nil
	}
	// then lookup immutable tables
	for i := range t.immutable {
		if v := t.immutable[i].Query(key); v != nil {
			return v.Value, nil
		}
	}

	// last lookup sparse index table
	for i := range t.sparseIndex {
		if t.sparseIndex[i].Key == key {
			disk, err := NewDiskSSTable(t.sparseIndex[i].TableName)
			if err != nil {
				return "", err
			}
			if v, err := disk.Query(t.sparseIndex[i].BlockIndex, t.sparseIndex[i].DataStart, key); err != nil {
				return "", err
			} else {
				return v.Value, nil
			}
		} else {
			disk, err := NewDiskSSTable(t.sparseIndex[i].TableName)
			if err != nil {
				return "", err
			}
			if v, err := disk.Query(t.sparseIndex[i].BlockIndex, t.sparseIndex[i].DataStart, key); err != nil {
				return "", err
			} else {
				return v.Value, nil
			}
		}
	}

	return "", errors.New("key not exists")
}

// Flush memory data to disk, generate a disk sstable
func (t *MEMSSTable) Flush() error {
	lz4buf := bytes.NewBuffer(nil)

	t.lock.Lock()
	t.switchTable()
	t.lock.Unlock()

	for len(t.immutable) > 0 {
		filename := fmt.Sprintf("%s/%d.sdb", t.rootPath, t.id)
		f, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			return fmt.Errorf("sstable.flush: err %v", err)
		}
		metaInfo := new(SSTableMetaInfo)
		metaInfo.Version = 1
		metaInfo.BlockKeyNum = t.blockKeyNum
		metaInfo.TableBlockNum = t.tableBlockNum
		sparseIndex := make([]SparseIndex, 0, int(t.tableBlockNum)*2)
		var i int
		for i = 0; i < len(t.immutable) && i < int(t.tableBlockNum); i++ {
			if t.immutable[i].Len() == 0 {
				continue
			}
			lz4buf.Reset()
			lz4w := lz4.NewWriter(lz4buf)
			_, body := t.immutable[i].Bytes()
			_, err := lz4w.Write(body)
			if err != nil {
				return err
			}
			lz4w.Close()
			binary.Write(f, binary.LittleEndian, uint32(lz4buf.Len()))
			io.Copy(f, lz4buf)
			sparseIndex = append(sparseIndex, SparseIndex{
				Key:        t.immutable[i].data[0].Key,
				DataStart:  uint32(metaInfo.DataLength),
				BlockIndex: uint32(i),
			})
			metaInfo.DataLength += uint64(lz4buf.Len()) + 4
		}

		// write sparse index
		metaInfo.IndexStart = metaInfo.DataLength
		for i := range sparseIndex {
			n, body := sparseIndex[i].Bytes()
			binary.Write(f, binary.LittleEndian, uint32(n))
			f.Write(body)
			metaInfo.IndexLength += uint64(n) + 4
		}

		// write meta info
		n, err := f.Write(metaInfo.Bytes())
		fmt.Printf("metainfo length=%d, %+v\n", n, metaInfo)
		if err != nil {
			return err
		}
		if err := f.Sync(); err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		if err := t.wal.Remove(); err != nil {
			return err
		}

		t.lock.Lock()
		if len(t.immutable) >= i {
			t.immutable = t.immutable[i:]
		} else {
			t.immutable = t.immutable[:0]
		}

		t.id++
		t.wal, _ = NewWAL(fmt.Sprintf("%s/%d.wal", t.rootPath, t.id))
		t.lock.Unlock()
	}

	return nil
}

// LoadFromDiskTable restore sstable from wal
func (t *MEMSSTable) LoadFromDiskTable(f *os.File) error {
	f.Seek(-40, io.SeekEnd)
	data := make([]byte, 40)
	nn, err := f.Read(data)
	if err != nil && err != io.EOF {
		return err
	}
	if nn != 40 {
		return fmt.Errorf("read metainfo length error: %d", nn)
	}

	metaInfo := new(SSTableMetaInfo)
	metaInfo.Restore(data)
	fmt.Printf("metainfo length=%d, %+v\n", nn, metaInfo)

	// restore sparse index
	f.Seek(-40-int64(metaInfo.IndexLength), io.SeekEnd)
	var n uint32
	for {
		if err = binary.Read(f, binary.LittleEndian, &n); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		if n == 0 {
			break
		}
		if cap(data) < int(n) {
			data = make([]byte, n)
		} else {
			data = data[:n]
		}
		if nn, err = f.Read(data); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		index := new(SparseIndex)
		index.Restore(data)
		index.TableName = f.Name()
		t.sparseIndex = append(t.sparseIndex, index)
		fmt.Println("load sparse index: ", index.Key, nn)
	}

	t.id++ // restore a table, need incrase file id
	return nil
}

// LoadFromWAL restore sstable from wal
func (t *MEMSSTable) LoadFromWAL(f io.ReadSeeker) error {
	var n uint32
	var err error
	var data []byte
	for {
		if err = binary.Read(f, binary.LittleEndian, &n); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		if n == 0 {
			break
		}
		if cap(data) < int(n) {
			data = make([]byte, n)
		} else {
			data = data[:n]
		}

		if _, err = f.Read(data); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		cmd := new(Command)
		cmd.Restore(data)
		if err = t.command(cmd, true); err != nil {
			return err
		}
	}

	return nil
}

// switchTable change current table to immutable, and create a new table for write
func (t *MEMSSTable) switchTable() {
	t.activeTable.Sort()
	t.immutable = append(t.immutable, t.activeTable)
	t.activeTable = NewSSTable()
}
