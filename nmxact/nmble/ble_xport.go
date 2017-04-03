package nmble

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	log "github.com/Sirupsen/logrus"

	"mynewt.apache.org/newt/util/unixchild"
	"mynewt.apache.org/newtmgr/nmxact/nmxutil"
	"mynewt.apache.org/newtmgr/nmxact/sesn"
)

type XportCfg struct {
	// Path of Unix domain socket to create and listen on.
	SockPath string

	// Path of the blehostd executable.
	BlehostdPath string

	// How long to wait for the blehostd process to connect to the Unix domain
	// socket.
	BlehostdAcceptTimeout time.Duration

	// Whether to restart the blehostd process if it terminates.
	BlehostdRestart bool

	// How long to wait for a JSON response from the blehostd process.
	BlehostdRspTimeout time.Duration

	// Path of the BLE controller device (e.g., /dev/ttyUSB0).
	DevPath string

	// How long to allow for the host and controller to sync at startup.
	SyncTimeout time.Duration
}

func NewXportCfg() XportCfg {
	return XportCfg{
		BlehostdAcceptTimeout: time.Second,
		BlehostdRspTimeout:    time.Second,
		BlehostdRestart:       true,
		SyncTimeout:           10 * time.Second,
	}
}

type BleXportState uint32

const (
	BLE_XPORT_STATE_STOPPED BleXportState = iota
	BLE_XPORT_STATE_STARTING
	BLE_XPORT_STATE_STARTED
	BLE_XPORT_STATE_STOPPING
)

// Implements xport.Xport.
type BleXport struct {
	Bd               *BleDispatcher
	client           *unixchild.Client
	state            BleXportState
	stopChan         chan struct{}
	shutdownChan     chan bool
	numStopListeners int

	cfg XportCfg
}

func NewBleXport(cfg XportCfg) (*BleXport, error) {
	bx := &BleXport{
		Bd:           NewBleDispatcher(),
		shutdownChan: make(chan bool),
		cfg:          cfg,
	}

	return bx, nil
}

func (bx *BleXport) createUnixChild() {
	config := unixchild.Config{
		SockPath:      bx.cfg.SockPath,
		ChildPath:     bx.cfg.BlehostdPath,
		ChildArgs:     []string{bx.cfg.DevPath, bx.cfg.SockPath},
		Depth:         10,
		MaxMsgSz:      10240,
		AcceptTimeout: bx.cfg.BlehostdAcceptTimeout,
	}

	bx.client = unixchild.New(config)
}

func (bx *BleXport) BuildSesn(cfg sesn.SesnCfg) (sesn.Sesn, error) {
	switch cfg.MgmtProto {
	case sesn.MGMT_PROTO_NMP:
		return NewBlePlainSesn(bx, cfg), nil
	case sesn.MGMT_PROTO_OMP:
		return NewBleOicSesn(bx, cfg), nil
	default:
		return nil, fmt.Errorf(
			"Invalid management protocol: %d; expected NMP or OMP",
			cfg.MgmtProto)
	}
}

func (bx *BleXport) addSyncListener() (*BleListener, error) {
	bl := NewBleListener()
	base := BleMsgBase{
		Op:         MSG_OP_EVT,
		Type:       MSG_TYPE_SYNC_EVT,
		Seq:        -1,
		ConnHandle: -1,
	}
	if err := bx.Bd.AddListener(base, bl); err != nil {
		return nil, err
	}

	return bl, nil
}

func (bx *BleXport) removeSyncListener() {
	base := BleMsgBase{
		Op:         MSG_OP_EVT,
		Type:       MSG_TYPE_SYNC_EVT,
		Seq:        -1,
		ConnHandle: -1,
	}
	bx.Bd.RemoveListener(base)
}

func (bx *BleXport) querySyncStatus() (bool, error) {
	req := &BleSyncReq{
		Op:   MSG_OP_REQ,
		Type: MSG_TYPE_SYNC,
		Seq:  NextSeq(),
	}

	j, err := json.Marshal(req)
	if err != nil {
		return false, err
	}

	bl := NewBleListener()
	base := BleMsgBase{
		Op:         -1,
		Type:       -1,
		Seq:        req.Seq,
		ConnHandle: -1,
	}
	if err := bx.Bd.AddListener(base, bl); err != nil {
		return false, err
	}
	defer bx.Bd.RemoveListener(base)

	bx.txNoSync(j)
	for {
		select {
		case err := <-bl.ErrChan:
			return false, err
		case bm := <-bl.BleChan:
			switch msg := bm.(type) {
			case *BleSyncRsp:
				return msg.Synced, nil
			}
		}
	}
}

func (bx *BleXport) initialSyncCheck() (bool, *BleListener, error) {
	bl, err := bx.addSyncListener()
	if err != nil {
		return false, nil, err
	}

	synced, err := bx.querySyncStatus()
	if err != nil {
		bx.removeSyncListener()
		return false, nil, err
	}

	return synced, bl, nil
}

func (bx *BleXport) shutdown(restart bool, err error) {
	var fullyStarted bool

	if bx.setStateFrom(BLE_XPORT_STATE_STARTED,
		BLE_XPORT_STATE_STOPPING) {

		fullyStarted = true
	} else if bx.setStateFrom(BLE_XPORT_STATE_STARTING,
		BLE_XPORT_STATE_STOPPING) {

		fullyStarted = false
	} else {
		// Stop already in progress.
		return
	}

	// Stop the unixchild instance (blehostd + socket).
	if bx.client != nil {
		bx.client.Stop()

		// Unblock the unixchild instance.
		bx.client.FromChild <- nil
	}

	// Indicate an error to all of this transport's listeners.  This prevents
	// them from blocking endlessly while awaiting a BLE message.
	bx.Bd.ErrorAll(err)

	// Stop all of this transport's go routines.
	for i := 0; i < bx.numStopListeners; i++ {
		bx.stopChan <- struct{}{}
	}

	bx.setStateFrom(BLE_XPORT_STATE_STOPPING, BLE_XPORT_STATE_STOPPED)

	// Indicate that the shutdown is complete.  If restarts are enabled on this
	// transport, this signals that the transport should be started again.
	if fullyStarted {
		bx.shutdownChan <- restart
	}
}

func (bx *BleXport) setStateFrom(from BleXportState, to BleXportState) bool {
	return atomic.CompareAndSwapUint32(
		(*uint32)(&bx.state), uint32(from), uint32(to))
}

func (bx *BleXport) getState() BleXportState {
	u32 := atomic.LoadUint32((*uint32)(&bx.state))
	return BleXportState(u32)
}

func (bx *BleXport) Stop() error {
	bx.shutdown(false, nil)
	return nil
}

func (bx *BleXport) startOnce() error {
	if !bx.setStateFrom(BLE_XPORT_STATE_STOPPED, BLE_XPORT_STATE_STARTING) {
		return nmxutil.NewXportError("BLE xport started twice")
	}

	bx.stopChan = make(chan struct{})
	bx.numStopListeners = 0
	bx.Bd.Clear()

	bx.createUnixChild()
	if err := bx.client.Start(); err != nil {
		if unixchild.IsUcAcceptError(err) {
			err = nmxutil.NewXportError(
				"blehostd did not connect to socket; " +
					"controller not attached?")
		} else {
			panic(err.Error())
			err = nmxutil.NewXportError(
				"Failed to start child process: " + err.Error())
		}
		bx.shutdown(true, err)
		return err
	}

	go func() {
		bx.numStopListeners++
		for {
			select {
			case err := <-bx.client.ErrChild:
				err = nmxutil.NewXportError("BLE transport error: " +
					err.Error())
				go bx.shutdown(true, err)

			case <-bx.stopChan:
				return
			}
		}
	}()

	go func() {
		bx.numStopListeners++
		for {
			select {
			case buf := <-bx.client.FromChild:
				if len(buf) != 0 {
					log.Debugf("Receive from blehostd:\n%s", hex.Dump(buf))
					bx.Bd.Dispatch(buf)
				}

			case <-bx.stopChan:
				return
			}
		}
	}()

	synced, bl, err := bx.initialSyncCheck()
	if err != nil {
		bx.shutdown(true, err)
		return err
	}

	if !synced {
		// Not synced yet.  Wait for sync event.

	SyncLoop:
		for {
			select {
			case err := <-bl.ErrChan:
				bx.shutdown(true, err)
				return err
			case bm := <-bl.BleChan:
				switch msg := bm.(type) {
				case *BleSyncEvt:
					if msg.Synced {
						break SyncLoop
					}
				}
			case <-time.After(bx.cfg.SyncTimeout):
				err := nmxutil.NewXportError(
					"Timeout waiting for host <-> controller sync")
				bx.shutdown(true, err)
				return err
			}
		}
	}

	// Host and controller are synced.  Listen for sync loss in the background.
	go func() {
		bx.numStopListeners++
		for {
			select {
			case err := <-bl.ErrChan:
				go bx.shutdown(true, err)
			case bm := <-bl.BleChan:
				switch msg := bm.(type) {
				case *BleSyncEvt:
					if !msg.Synced {
						go bx.shutdown(true, nmxutil.NewXportError(
							"BLE host <-> controller sync lost"))
					}
				}
			case <-bx.stopChan:
				return
			}
		}
	}()

	if !bx.setStateFrom(BLE_XPORT_STATE_STARTING, BLE_XPORT_STATE_STARTED) {
		bx.shutdown(true, err)
		return nmxutil.NewXportError(
			"Internal error; BLE transport in unexpected state")
	}

	return nil
}

func (bx *BleXport) Start() error {
	// Try to start the transport.  If this first attempt fails, report the
	// error and don't retry.
	if err := bx.startOnce(); err != nil {
		log.Debugf("Error starting BLE transport: %s",
			err.Error())
		return err
	}

	// Now that the first start attempt has succeeded, start a restart loop in
	// the background.
	go func() {
		// Block until transport shuts down.
		restart := <-bx.shutdownChan
		for {
			// If restarts are disabled, or if the shutdown was a result of an
			// explicit stop call (instead of an unexpected error), stop
			// restarting the transport.
			if !bx.cfg.BlehostdRestart || !restart {
				break
			}

			// Wait a second before the next restart.  This is necessary to
			// ensure the unix domain socket can be rebound.
			time.Sleep(time.Second)

			// Attempt to start the transport again.
			if err := bx.startOnce(); err != nil {
				// Start attempt failed.
				log.Debugf("Error starting BLE transport: %s",
					err.Error())
			} else {
				// Success.  Block until the transport shuts down.
				restart = <-bx.shutdownChan
			}
		}
	}()

	return nil
}

func (bx *BleXport) txNoSync(data []byte) {
	log.Debugf("Tx to blehostd:\n%s", hex.Dump(data))
	bx.client.ToChild <- data
}

func (bx *BleXport) Tx(data []byte) error {
	if bx.getState() != BLE_XPORT_STATE_STARTED {
		return nmxutil.NewXportError(
			fmt.Sprintf("Attempt to transmit before BLE xport fully started; "+
				"state=%d", bx.getState()))
	}

	bx.txNoSync(data)
	return nil
}

func (bx *BleXport) RspTimeout() time.Duration {
	return bx.cfg.BlehostdRspTimeout
}
