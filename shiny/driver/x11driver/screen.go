// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package x11driver

import (
	"fmt"
	"image"
	"log"
	"sync"

	"github.com/BurntSushi/xgb"
	"github.com/BurntSushi/xgb/render"
	"github.com/BurntSushi/xgb/shm"
	"github.com/BurntSushi/xgb/xproto"

	"golang.org/x/exp/shiny/driver/internal/pump"
	"golang.org/x/exp/shiny/screen"
)

// TODO: check that xgb is safe to use concurrently from multiple goroutines.
// For example, its Conn.WaitForEvent concept is a method, not a channel, so
// it's not obvious how to interrupt it to service a NewWindow request.

type completion struct {
	sender screen.Sender
	event  screen.UploadedEvent
}

type screenImpl struct {
	xc  *xgb.Conn
	xsi *xproto.ScreenInfo

	atomWMDeleteWindow xproto.Atom
	atomWMProtocols    xproto.Atom
	atomWMTakeFocus    xproto.Atom

	pictformat24 render.Pictformat
	pictformat32 render.Pictformat

	// window32 and its related X11 resources is an unmapped window so that we
	// have a depth-32 window to create depth-32 pixmaps from, i.e. pixmaps
	// with an alpha channel. The root window isn't guaranteed to be depth-32.
	gcontext32 xproto.Gcontext
	window32   xproto.Window

	mu      sync.Mutex
	buffers map[shm.Seg]*bufferImpl
	uploads map[uint16]completion
	windows map[xproto.Window]*windowImpl
}

func newScreenImpl(xc *xgb.Conn) (*screenImpl, error) {
	s := &screenImpl{
		xc:      xc,
		xsi:     xproto.Setup(xc).DefaultScreen(xc),
		buffers: map[shm.Seg]*bufferImpl{},
		uploads: map[uint16]completion{},
		windows: map[xproto.Window]*windowImpl{},
	}
	if err := s.initAtoms(); err != nil {
		return nil, err
	}
	if err := s.initPictformats(); err != nil {
		return nil, err
	}
	if err := s.initWindow32(); err != nil {
		return nil, err
	}
	go s.run()
	return s, nil
}

func (s *screenImpl) run() {
	for {
		ev, err := s.xc.WaitForEvent()
		if err != nil {
			log.Printf("x11driver: xproto.WaitForEvent: %v", err)
			continue
		}

		xw, destroy := xproto.Window(0), false
		switch ev := ev.(type) {
		default:
			continue
		case shm.CompletionEvent:
			s.handleCompletion(ev)
			continue
		case xproto.ClientMessageEvent:
			xw = ev.Window
		case xproto.ConfigureNotifyEvent:
			xw = ev.Window
		case xproto.DestroyNotifyEvent:
			xw = ev.Window
			destroy = true
		case xproto.ExposeEvent:
			xw = ev.Window
		case xproto.FocusInEvent:
			xw = ev.Event
		case xproto.FocusOutEvent:
			xw = ev.Event
		case xproto.KeyPressEvent:
			xw = ev.Event
		case xproto.KeyReleaseEvent:
			xw = ev.Event
		case xproto.ButtonPressEvent:
			xw = ev.Event
		case xproto.ButtonReleaseEvent:
			xw = ev.Event
		case xproto.MotionNotifyEvent:
			xw = ev.Event
		}

		s.mu.Lock()
		w := s.windows[xw]
		if destroy {
			delete(s.windows, xw)
		}
		s.mu.Unlock()

		if w == nil {
			log.Printf("x11driver: no window found for event %T", ev)
			continue
		}
		if destroy {
			close(w.xevents)
		} else {
			w.xevents <- ev
		}
	}
}

// TODO: is findBuffer and the s.buffers field unused? Delete?

func (s *screenImpl) findBuffer(key shm.Seg) *bufferImpl {
	s.mu.Lock()
	b := s.buffers[key]
	s.mu.Unlock()
	return b
}

func (s *screenImpl) handleCompletion(ev shm.CompletionEvent) {
	s.mu.Lock()
	completion, ok := s.uploads[ev.Sequence]
	s.mu.Unlock()

	if !ok {
		log.Printf("x11driver: no matching upload for a SHM completion event")
		return
	}
	completion.event.Buffer.(*bufferImpl).postUpload()
	if completion.sender != nil {
		// Call Send in a separate goroutine, so that this event-handling
		// goroutine doesn't block.
		go completion.sender.Send(completion.event)
	}
}

const (
	maxShmSide = 0x00007fff // 32,767 pixels.
	maxShmSize = 0x10000000 // 268,435,456 bytes.
)

func (s *screenImpl) NewBuffer(size image.Point) (retBuf screen.Buffer, retErr error) {
	// TODO: detect if the X11 server or connection cannot support SHM pixmaps,
	// and fall back to regular pixmaps.

	w, h := int64(size.X), int64(size.Y)
	if w < 0 || maxShmSide < w || h < 0 || maxShmSide < h || maxShmSize < 4*w*h {
		return nil, fmt.Errorf("x11driver: invalid buffer size %v", size)
	}
	xs, err := shm.NewSegId(s.xc)
	if err != nil {
		return nil, fmt.Errorf("x11driver: shm.NewSegId: %v", err)
	}

	bufLen := 4 * size.X * size.Y
	shmid, addr, err := shmOpen(bufLen)
	if err != nil {
		return nil, fmt.Errorf("x11driver: shmOpen: %v", err)
	}
	defer func() {
		if retErr != nil {
			shmClose(addr)
		}
	}()
	a := (*[maxShmSize]byte)(addr)
	buf := (*a)[:bufLen:bufLen]

	// readOnly is whether the shared memory is read-only from the X11 server's
	// point of view. We need false to use SHM pixmaps.
	const readOnly = false
	shm.Attach(s.xc, xs, uint32(shmid), readOnly)

	b := &bufferImpl{
		s:    s,
		addr: addr,
		buf:  buf,
		rgba: image.RGBA{
			Pix:    buf,
			Stride: 4 * size.X,
			Rect:   image.Rectangle{Max: size},
		},
		size: size,
		xs:   xs,
	}

	s.mu.Lock()
	s.buffers[b.xs] = b
	s.mu.Unlock()

	return b, nil
}

func (s *screenImpl) NewTexture(size image.Point) (screen.Texture, error) {
	w, h := int64(size.X), int64(size.Y)
	if w < 0 || maxShmSide < w || h < 0 || maxShmSide < h || maxShmSize < 4*w*h {
		return nil, fmt.Errorf("x11driver: invalid texture size %v", size)
	}

	xm, err := xproto.NewPixmapId(s.xc)
	if err != nil {
		return nil, fmt.Errorf("x11driver: xproto.NewPixmapId failed: %v", err)
	}
	xp, err := render.NewPictureId(s.xc)
	if err != nil {
		return nil, fmt.Errorf("x11driver: xproto.NewPictureId failed: %v", err)
	}

	t := &textureImpl{
		s:    s,
		size: size,
		xm:   xm,
		xp:   xp,
	}

	xproto.CreatePixmap(s.xc, textureDepth, xm, xproto.Drawable(s.window32), uint16(w), uint16(h))
	render.CreatePicture(s.xc, xp, xproto.Drawable(xm), s.pictformat32, 0, nil)
	return t, nil
}

func (s *screenImpl) NewWindow(opts *screen.NewWindowOptions) (screen.Window, error) {
	// TODO: look at opts.
	const width, height = 1024, 768

	xw, err := xproto.NewWindowId(s.xc)
	if err != nil {
		return nil, fmt.Errorf("x11driver: xproto.NewWindowId failed: %v", err)
	}
	xg, err := xproto.NewGcontextId(s.xc)
	if err != nil {
		return nil, fmt.Errorf("x11driver: xproto.NewGcontextId failed: %v", err)
	}
	xp, err := render.NewPictureId(s.xc)
	if err != nil {
		return nil, fmt.Errorf("x11driver: render.NewPictureId failed: %v", err)
	}
	pictformat := render.Pictformat(0)
	switch s.xsi.RootDepth {
	default:
		return nil, fmt.Errorf("x11driver: unsupported root depth %d", s.xsi.RootDepth)
	case 24:
		pictformat = s.pictformat24
	case 32:
		pictformat = s.pictformat32
	}

	w := &windowImpl{
		s:       s,
		xw:      xw,
		xg:      xg,
		xp:      xp,
		pump:    pump.Make(),
		xevents: make(chan xgb.Event),
	}
	go w.run()

	s.mu.Lock()
	s.windows[xw] = w
	s.mu.Unlock()

	xproto.CreateWindow(s.xc, s.xsi.RootDepth, xw, s.xsi.Root,
		0, 0, width, height, 0,
		xproto.WindowClassInputOutput, s.xsi.RootVisual,
		xproto.CwEventMask,
		[]uint32{0 |
			xproto.EventMaskKeyPress |
			xproto.EventMaskKeyRelease |
			xproto.EventMaskButtonPress |
			xproto.EventMaskButtonRelease |
			xproto.EventMaskPointerMotion |
			xproto.EventMaskExposure |
			xproto.EventMaskStructureNotify |
			xproto.EventMaskFocusChange,
		},
	)
	s.setProperty(xw, s.atomWMProtocols, s.atomWMDeleteWindow, s.atomWMTakeFocus)
	xproto.CreateGC(s.xc, xg, xproto.Drawable(xw), 0, nil)
	render.CreatePicture(s.xc, xp, xproto.Drawable(xw), pictformat, 0, nil)
	xproto.MapWindow(s.xc, xw)

	return w, nil
}

func (s *screenImpl) initAtoms() (err error) {
	s.atomWMDeleteWindow, err = s.internAtom("WM_DELETE_WINDOW")
	if err != nil {
		return err
	}
	s.atomWMProtocols, err = s.internAtom("WM_PROTOCOLS")
	if err != nil {
		return err
	}
	s.atomWMTakeFocus, err = s.internAtom("WM_TAKE_FOCUS")
	if err != nil {
		return err
	}
	return nil
}

func (s *screenImpl) internAtom(name string) (xproto.Atom, error) {
	r, err := xproto.InternAtom(s.xc, false, uint16(len(name)), name).Reply()
	if err != nil {
		return 0, fmt.Errorf("x11driver: xproto.InternAtom failed: %v", err)
	}
	if r == nil {
		return 0, fmt.Errorf("x11driver: xproto.InternAtom failed")
	}
	return r.Atom, nil
}

func (s *screenImpl) initPictformats() error {
	pformats, err := render.QueryPictFormats(s.xc).Reply()
	if err != nil {
		return fmt.Errorf("x11driver: render.QueryPictFormats failed: %v", err)
	}
	s.pictformat24, err = findPictformat(pformats.Formats, 24)
	if err != nil {
		return err
	}
	s.pictformat32, err = findPictformat(pformats.Formats, 32)
	if err != nil {
		return err
	}
	return nil
}

func findPictformat(fs []render.Pictforminfo, depth byte) (render.Pictformat, error) {
	// This presumes little-endian BGRA.
	want := render.Directformat{
		RedShift:   16,
		RedMask:    0xff,
		GreenShift: 8,
		GreenMask:  0xff,
		BlueShift:  0,
		BlueMask:   0xff,
		AlphaShift: 24,
		AlphaMask:  0xff,
	}
	if depth == 24 {
		want.AlphaShift = 0
		want.AlphaMask = 0x00
	}
	for _, f := range fs {
		if f.Type == render.PictTypeDirect && f.Depth == depth && f.Direct == want {
			return f.Id, nil
		}
	}
	return 0, fmt.Errorf("x11driver: no matching Pictformat for depth %d", depth)
}

func (s *screenImpl) initWindow32() error {
	visualid, err := findVisual(s.xsi, 32)
	if err != nil {
		return err
	}
	colormap, err := xproto.NewColormapId(s.xc)
	if err != nil {
		return fmt.Errorf("x11driver: xproto.NewColormapId failed: %v", err)
	}
	if err := xproto.CreateColormapChecked(
		s.xc, xproto.ColormapAllocNone, colormap, s.xsi.Root, visualid).Check(); err != nil {
		return fmt.Errorf("x11driver: xproto.CreateColormap failed: %v", err)
	}
	s.window32, err = xproto.NewWindowId(s.xc)
	if err != nil {
		return fmt.Errorf("x11driver: xproto.NewWindowId failed: %v", err)
	}
	s.gcontext32, err = xproto.NewGcontextId(s.xc)
	if err != nil {
		return fmt.Errorf("x11driver: xproto.NewGcontextId failed: %v", err)
	}
	const depth = 32
	xproto.CreateWindow(s.xc, depth, s.window32, s.xsi.Root,
		0, 0, 1, 1, 0,
		xproto.WindowClassInputOutput, visualid,
		// The CwBorderPixel attribute seems necessary for depth == 32. See
		// http://stackoverflow.com/questions/3645632/how-to-create-a-window-with-a-bit-depth-of-32
		xproto.CwBorderPixel|xproto.CwColormap,
		[]uint32{0, uint32(colormap)},
	)
	xproto.CreateGC(s.xc, s.gcontext32, xproto.Drawable(s.window32), 0, nil)
	return nil
}

func findVisual(xsi *xproto.ScreenInfo, depth byte) (xproto.Visualid, error) {
	for _, d := range xsi.AllowedDepths {
		if d.Depth != depth {
			continue
		}
		for _, v := range d.Visuals {
			if v.RedMask == 0xff0000 && v.GreenMask == 0xff00 && v.BlueMask == 0xff {
				return v.VisualId, nil
			}
		}
	}
	return 0, fmt.Errorf("x11driver: no matching Visualid")
}

func (s *screenImpl) setProperty(xw xproto.Window, prop xproto.Atom, values ...xproto.Atom) {
	b := make([]byte, len(values)*4)
	for i, v := range values {
		b[4*i+0] = uint8(v >> 0)
		b[4*i+1] = uint8(v >> 8)
		b[4*i+2] = uint8(v >> 16)
		b[4*i+3] = uint8(v >> 24)
	}
	xproto.ChangeProperty(s.xc, xproto.PropModeReplace, xw, prop, xproto.AtomAtom, 32, uint32(len(values)), b)
}