package shaping

import (
	"io"

	"github.com/op/go-logging"

	"github.com/blanu/Dust/go/Dust/crypting"
	"github.com/blanu/Dust/go/Dust/proc"
)

var log = logging.MustGetLogger("Dust/shaper")

const (
	shaperBufSize = 1024
)

// Params represents globally applicable options for the shaper itself.  The zero value is the default.
type Params struct {
	IgnoreDuration bool
}

func (params *Params) Validate() error {
	return nil
}

// Shaper represents a process mediating between a shaped channel and a Dust crypting session.  It can be
// managed through its proc.Link structure.
type Shaper struct {
	proc.Ctl
	Params

	crypter   *crypting.Session
	shapedIn  io.Reader
	shapedOut io.Writer
	closer    io.Closer

	reader  reader
	decoder Decoder
	inBuf   []byte
	pushBuf []byte
	reading bool

	timer    timer
	writer   writer
	encoder  Encoder
	outBuf   []byte
	outPoint int
	outMark  int
	pullBuf  []byte
	pullMark int
	writing  bool
}

func (sh *Shaper) handleRead(subn int) error {
	sh.reading = false
	in := sh.inBuf[:subn]

	for {
		dn, sn := sh.decoder.UnshapeBytes(sh.pushBuf, in)
		log.Debug("  <- unshaped %d from %d bytes", dn, sn)
		if dn > 0 {
			_, err := sh.crypter.PushRead(sh.pushBuf[:dn])
			if err != nil {
				return err
			}
		}

		in = in[sn:]
		if len(in) == 0 {
			break
		}
	}

	if sh.crypter.PushReadCTS() {
		sh.reader.cycle(sh.inBuf[:])
		sh.reading = true
	} else {
		log.Debug("  <- propagating !CTS backpressure from front")
	}

	return nil
}

func (sh *Shaper) handleWrite(subn int) error {
	sh.outPoint += subn
	switch {
	case sh.outPoint < sh.outMark:
		log.Debug("-> only wrote %d bytes, %d left over", subn, sh.outMark-sh.outPoint)
		sh.writer.cycle(sh.outBuf[sh.outPoint:sh.outMark])
	case sh.outPoint == sh.outMark:
		sh.writing = false
	case sh.outPoint > sh.outMark:
		panic("Dust/shaper: somehow wrote more bytes than we had")
	}

	return nil
}

func (sh *Shaper) handleTimer() error {
	if !sh.reading {
		// We stopped reading to propagate backpressure.  See whether the application is consuming
		// anything by now.  TODO: this isn't really the right place to do this; we want a separate
		// notification, but that implies making crypting.Front capable of either blocking or
		// nonblocking operation, aaargh.
		_, err := sh.crypter.PushRead(nil)
		if err != nil {
			return err
		}

		if sh.crypter.PushReadCTS() {
			log.Debug("  <- releasing backpressure propagation")
			sh.reader.cycle(sh.inBuf[:])
			sh.reading = true
		}
	}

	// This must come before checks for whether to skip packets, so that we always get the next
	// timer pulse.
	sh.timer.cycle(sh.encoder.NextPacketSleep())

	if sh.writing {
		// We're under visible stream backpressure, so just skip it.
		log.Debug("-> cannot write any more right now")
		return nil
	}

	outLen := int(sh.encoder.NextPacketLength())

	outMark := 0
	out := sh.outBuf[:outLen]
	for outMark < outLen {
		if sh.pullMark == 0 {
			req := outLen - outMark
			if req >= len(sh.pullBuf)-sh.pullMark {
				req = len(sh.pullBuf) - sh.pullMark
			}

			pulled, err := sh.crypter.PullWrite(sh.pullBuf[sh.pullMark : sh.pullMark+req])
			if err != nil {
				return err
			}

			sh.pullMark += pulled
		}

		dn, sn := sh.encoder.ShapeBytes(out[outMark:], sh.pullBuf[:sh.pullMark])
		log.Debug("-> shaped %d from %d bytes", dn, sn)
		outMark += dn
		copy(sh.pullBuf, sh.pullBuf[sn:sh.pullMark])
		sh.pullMark -= sn
	}

	sh.outPoint = 0
	sh.outMark = outMark
	sh.writer.cycle(sh.outBuf[sh.outPoint:sh.outMark])
	sh.writing = true
	return nil
}

func (sh *Shaper) runShaper(env *proc.Env) (err error) {
	defer func() {
		if sh.closer != nil {
			sh.closer.Close()
		}
		log.Info("shaper exiting")
	}()

	sh.reader.cycle(sh.inBuf[:])
	sh.reading = true
	sh.timer.cycle(sh.encoder.NextPacketSleep())
	log.Info("shaper starting")

	for {
		// Make sure the reader never starves.  AHAHAHAHA Go doesn't let you separate readiness polling
		// on channels from reads, does it?  So you have to replicate all the code.  Ahahahahaha.
		select {
		case subn := <-sh.reader.Rep:
			err = sh.handleRead(subn.(int))
			if err != nil {
				return
			}
		default:
		}

		// XXX: see above
		// TODO: frequently zero-sleeping models don't seem to work here.  It seems the select can
		// repeatedly select timed writes to perform and never reads, and then neither side of the
		// connection makes any progress.  If we had access to the polling loop we could do some
		// rudimentary source round-robining, but nope!  Nope!  We get to maybe hardcode repetitious
		// channel priority stuff to try to resolve this later because none of these waits are
		// composable!  Thanks, Go!  Thanks a WHOLE BUNCH.
		select {
		case subn := <-sh.reader.Rep:
			err = sh.handleRead(subn.(int))
			if err != nil {
				return
			}

		case subn := <-sh.writer.Rep:
			err = sh.handleWrite(subn.(int))
			if err != nil {
				return
			}

		case _ = <-sh.timer.Rep:
			err = sh.handleTimer()
			if err != nil {
				return
			}

		case _ = <-env.Cancel:
			return env.ExitCanceled()
		}
	}
}

// NewShaper initializes a new shaper process object for the outward-facing side of crypter, using in/out for
// receiving and sending shaped data and decoder/encoder as the model for this side of the Dust connection.
// The shaper will not be running.  Call Start() on the shaper afterwards to start it in the background; after
// that point, the shaper takes responsibility for closing the closer if it is not nil.
func NewShaper(
	parent *proc.Env,
	crypter *crypting.Session,
	in io.Reader,
	decoder Decoder,
	out io.Writer,
	encoder Encoder,
	closer io.Closer,
	params *Params,
) (*Shaper, error) {
	sh := &Shaper{
		crypter:   crypter,
		shapedIn:  in,
		shapedOut: out,
		closer:    closer,

		// reader is initialized below
		decoder: decoder,
		inBuf:   make([]byte, shaperBufSize),
		pushBuf: make([]byte, shaperBufSize),

		// timer is initialized below
		encoder:  encoder,
		outBuf:   make([]byte, encoder.MaxPacketLength()),
		pullBuf:  make([]byte, shaperBufSize),
		pullMark: 0,
	}

	if params != nil {
		sh.Params = *params
	}

	env := proc.InitChild(parent, &sh.Ctl, sh.runShaper)
	sh.reader.Init(env, sh.shapedIn)
	sh.timer.Init(env)
	sh.writer.Init(env, sh.shapedOut)
	if !sh.IgnoreDuration {
		sh.timer.setDuration(sh.encoder.WholeStreamDuration())
	}
	return sh, nil
}
