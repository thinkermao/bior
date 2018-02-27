package raft

import (
	"encoding/binary"
	"fmt"

	log "github.com/sirupsen/logrus"
	"github.com/thinkermao/bior/raft/proto"
)

// implements of raft.Application interface.

func (app *application) ApplyEntry(entry *raftpd.Entry) {
	log.Debugf("[test] id: %d apply entry: %v", app.id, entry)

	var err error

	bytes := [8]byte{}
	value := int(binary.LittleEndian.Uint64(bytes[:]))
	index := int(entry.Index)

	err = app.callback.CheckApply(app.ID(), index, value)

	app.logMutex.Lock()
	defer app.logMutex.Unlock()
	if err == nil {
		if lastValue, ok := app.logs[index]; !ok {
			app.logs[index] = value
		} else {
			err = fmt.Errorf("%d apply same index: %d twice : %d, last: %d",
				app.id, index, value, lastValue)
		}
	}
	if err != nil {
		app.applyErr = err
	}
}

func (app *application) ReadStateNotice(idx uint64, bytes []byte) {
	app.logMutex.Lock()
	app.logMutex.Unlock()

	// TODO:
}

func (app *application) ApplySnapshot(snapshot *raftpd.Snapshot) {
	persist := app.getPersist()

	if persist == nil {
		panic("apply snapshot, but persist is nil")
	}

	persist.SaveSnapshot(snapshot)
}

func (app *application) ReadSnapshot() *raftpd.Snapshot {
	persist := app.getPersist()

	if persist == nil {
		panic("read snapshot, but persist is nil")
	}

	return persist.ReadSnapshot()
}
