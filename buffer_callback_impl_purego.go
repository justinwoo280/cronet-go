//go:build with_purego

package cronet

import (
	"sync"
	"sync/atomic"

	"github.com/sagernet/cronet-go/internal/cronet"

	"github.com/ebitengine/purego"
)

type bufferCallbackEntry struct {
	callback  BufferCallbackFunc
	destroyed atomic.Bool
}

var (
	bufferCallbackAccess    sync.RWMutex
	bufferCallbackMap       map[uintptr]*bufferCallbackEntry
	bufferCallbackOnDestroy uintptr
)

func init() {
	bufferCallbackMap = make(map[uintptr]*bufferCallbackEntry)
	bufferCallbackOnDestroy = purego.NewCallback(onBufferDestroyCallback)
}

func onBufferDestroyCallback(self, buffer uintptr) uintptr {
	bufferCallbackAccess.RLock()
	entry := bufferCallbackMap[self]
	bufferCallbackAccess.RUnlock()
	if entry == nil || entry.destroyed.Load() {
		return 0 // Post-destroy callback, silently ignore
	}
	// Remove ownership before calling user code, which may destroy this handle
	// and allocate another callback at the same native address.
	bufferCallbackAccess.Lock()
	delete(bufferCallbackMap, self)
	bufferCallbackAccess.Unlock()
	if entry.callback != nil {
		entry.callback(BufferCallback{self}, Buffer{buffer})
	}
	return 0
}

func NewBufferCallback(callbackFunc BufferCallbackFunc) BufferCallback {
	ptr := cronet.BufferCallbackCreateWith(bufferCallbackOnDestroy)
	if callbackFunc != nil {
		bufferCallbackAccess.Lock()
		bufferCallbackMap[ptr] = &bufferCallbackEntry{callback: callbackFunc}
		bufferCallbackAccess.Unlock()
	}
	return BufferCallback{ptr}
}

func (c BufferCallback) destroy() {
	bufferCallbackAccess.Lock()
	entry := bufferCallbackMap[c.ptr]
	if entry != nil {
		entry.destroyed.Store(true)
	}
	delete(bufferCallbackMap, c.ptr)
	bufferCallbackAccess.Unlock()
}
