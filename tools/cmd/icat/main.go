// License: GPLv3 Copyright: 2022, Kovid Goyal, <kovid at kovidgoyal.net>

package icat

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"kitty/tools/cli"
	"kitty/tools/tty"
	"kitty/tools/tui"
	"kitty/tools/tui/graphics"
	"kitty/tools/tui/loop"
	"kitty/tools/utils"
	"kitty/tools/utils/images"
	"kitty/tools/utils/shm"
	"kitty/tools/utils/style"
)

var _ = fmt.Print

type Place struct {
	width, height, left, top int
}

var opts *Options
var lp *loop.Loop
var place *Place
var z_index int32
var remove_alpha *images.NRGBColor
var flip, flop bool

type transfer_mode int

const (
	unknown transfer_mode = iota
	unsupported
	supported
)

var transfer_by_file, transfer_by_memory, transfer_by_stream transfer_mode

var temp_files_to_delete []string
var shm_files_to_delete []shm.MMap
var direct_query_id, file_query_id, memory_query_id uint32
var stderr_is_tty bool
var query_in_flight bool
var stream_response string
var files_channel chan input_arg
var output_channel chan *image_data
var num_of_items int
var keep_going *atomic.Bool
var screen_size loop.ScreenSize

func parse_mirror() (err error) {
	flip = opts.Mirror == "both" || opts.Mirror == "vertical"
	flop = opts.Mirror == "both" || opts.Mirror == "horizontal"
	return
}

func parse_background() (err error) {
	if opts.Background == "" || opts.Background == "none" {
		return nil
	}
	col, err := style.ParseColor(opts.Background)
	if err != nil {
		return fmt.Errorf("Invalid value for --background: %w", err)
	}
	remove_alpha = &images.NRGBColor{R: col.Red, G: col.Green, B: col.Blue}
	return
}

func parse_z_index() (err error) {
	val := opts.ZIndex
	var origin int32
	if strings.HasPrefix(val, "--") {
		origin = -1073741824
		val = val[1:]
	}
	i, err := strconv.ParseInt(val, 10, 32)
	if err != nil {
		return fmt.Errorf("Invalid value for --z-index with error: %w", err)
	}
	z_index = int32(i) + origin
	return
}

func parse_place() (err error) {
	if opts.Place == "" {
		return nil
	}
	area, pos, found := utils.Cut(opts.Place, "@")
	if !found {
		return fmt.Errorf("Invalid --place specification: %s", opts.Place)
	}
	w, h, found := utils.Cut(area, "x")
	if !found {
		return fmt.Errorf("Invalid --place specification: %s", opts.Place)
	}
	l, t, found := utils.Cut(pos, "x")
	if !found {
		return fmt.Errorf("Invalid --place specification: %s", opts.Place)
	}
	place = &Place{}
	place.width, err = strconv.Atoi(w)
	if err != nil {
		return err
	}
	place.height, err = strconv.Atoi(h)
	if err != nil {
		return err
	}
	place.left, err = strconv.Atoi(l)
	if err != nil {
		return err
	}
	place.top, err = strconv.Atoi(t)
	if err != nil {
		return err
	}
	return nil
}

func print_error(format string, args ...any) {
	if lp == nil || !stderr_is_tty {
		fmt.Fprintf(os.Stderr, format, args...)
		fmt.Fprintln(os.Stderr)
	} else {
		lp.QueueWriteString("\r")
		lp.ClearToEndOfLine()
		for _, line := range utils.Splitlines(fmt.Sprintf(format, args...)) {
			lp.Println(line)
		}
	}
}

func on_detect_timeout(timer_id loop.IdType) error {
	if query_in_flight {
		return fmt.Errorf("Timed out waiting for a response form the terminal")
	}
	return nil
}

func on_initialize() (string, error) {
	var iid uint32
	sz, err := lp.ScreenSize()
	if err != nil {
		return "", fmt.Errorf("Failed to query terminal for screen size with error: %w", err)
	}
	if sz.WidthPx == 0 || sz.HeightPx == 0 {
		return "", fmt.Errorf("Terminal does not support reporting screen sizes in pixels, use a terminal such as kitty, WezTerm, Konsole, etc. that does.")
	}
	if opts.Clear {
		cc := &graphics.GraphicsCommand{}
		cc.SetAction(graphics.GRT_action_delete).SetDelete(graphics.GRT_free_visible)
		cc.WriteWithPayloadToLoop(lp, nil)
	}
	lp.AddTimer(time.Duration(opts.DetectionTimeout*float64(time.Second)), false, on_detect_timeout)
	g := func(t graphics.GRT_t, payload string) uint32 {
		iid += 1
		g1 := &graphics.GraphicsCommand{}
		g1.SetTransmission(t).SetAction(graphics.GRT_action_query).SetImageId(iid).SetDataWidth(1).SetDataHeight(1).SetFormat(
			graphics.GRT_format_rgb).SetDataSize(uint64(len(payload)))
		g1.WriteWithPayloadToLoop(lp, utils.UnsafeStringToBytes(payload))
		return iid
	}
	keep_going.Store(true)
	screen_size = sz
	if !opts.DetectSupport && num_of_items > 0 {
		num_workers := utils.Max(1, utils.Min(num_of_items, runtime.NumCPU()))
		for i := 0; i < num_workers; i++ {
			go run_worker()
		}
	}
	if opts.TransferMode != "detect" {
		return "", nil
	}

	query_in_flight = true
	direct_query_id = g(graphics.GRT_transmission_direct, "123")
	tf, err := graphics.CreateTempInRAM()
	if err == nil {
		file_query_id = g(graphics.GRT_transmission_tempfile, tf.Name())
		temp_files_to_delete = append(temp_files_to_delete, tf.Name())
		tf.Write([]byte{1, 2, 3})
		tf.Close()
	} else {
		transfer_by_file = unsupported
		print_error("Failed to create temporary file for data transfer, file based transfer is disabled. Error: %v", err)
	}
	sf, err := shm.CreateTemp("icat-", 3)
	if err == nil {
		memory_query_id = g(graphics.GRT_transmission_sharedmem, sf.Name())
		shm_files_to_delete = append(shm_files_to_delete, sf)
		copy(sf.Slice(), []byte{1, 2, 3})
		sf.Close()
	} else {
		transfer_by_memory = unsupported
		var ens *shm.ErrNotSupported
		if !errors.As(err, &ens) {
			print_error("Failed to create SHM for data transfer, memory based transfer is disabled. Error: %v", err)
		}
	}
	lp.QueueWriteString("\x1b[c")

	return "", nil
}

func on_query_finished() (err error) {
	query_in_flight = false
	if transfer_by_stream != supported {
		return fmt.Errorf("This terminal emulator does not support the graphics protocol, use a terminal emulator such as kitty that does support it")
	}
	if opts.DetectSupport {
		switch {
		case transfer_by_memory == supported:
			print_error("memory")
		case transfer_by_file == supported:
			print_error("file")
		default:
			print_error("stream")
		}
		quit_loop()
		return
	}
	return on_wakeup()
}

func on_query_response(g *graphics.GraphicsCommand) (err error) {
	var tm *transfer_mode
	switch g.ImageId() {
	case direct_query_id:
		tm = &transfer_by_stream
	case file_query_id:
		tm = &transfer_by_file
	case memory_query_id:
		tm = &transfer_by_memory
	}
	if g.ResponseMessage() == "OK" {
		*tm = supported
	} else {
		*tm = unsupported
	}
	return
}

func on_escape_code(etype loop.EscapeCodeType, payload []byte) (err error) {
	switch etype {
	case loop.CSI:
		if len(payload) > 3 && payload[0] == '?' && payload[len(payload)-1] == 'c' {
			return on_query_finished()
		}
	case loop.APC:
		g := graphics.GraphicsCommandFromAPC(payload)
		if g != nil {
			if query_in_flight {
				return on_query_response(g)
			}
		}
	}
	return
}

func on_finalize() string {
	if len(temp_files_to_delete) > 0 && transfer_by_file != supported {
		for _, name := range temp_files_to_delete {
			os.Remove(name)
		}
	}
	if len(shm_files_to_delete) > 0 && transfer_by_memory != supported {
		for _, name := range shm_files_to_delete {
			name.Unlink()
		}
	}
	return ""
}

var errors_occurred bool = false

func quit_loop() {
	if errors_occurred {
		lp.Quit(1)
	} else {
		lp.Quit(0)
	}
}

func on_wakeup() error {
	if query_in_flight {
		return nil
	}
	have_more := true
	for have_more {
		select {
		case imgd := <-output_channel:
			num_of_items--
			if imgd.err != nil {
				print_error("Failed to process \x1b[31m%s\x1b[39m: %v\r\n", imgd.source_name, imgd.err)
			} else {
				transmit_image(imgd)
			}
		default:
			have_more = false
		}
	}
	if num_of_items <= 0 && !query_in_flight {
		quit_loop()
	}
	return nil
}

func on_key_event(event *loop.KeyEvent) error {
	if event.MatchesPressOrRepeat("ctrl+c") {
		event.Handled = true
		if query_in_flight {
			print_error("Waiting for response from terminal, aborting now could lead to corruption")
			return nil
		}
		return fmt.Errorf("Aborted by user")
	}
	if event.MatchesPressOrRepeat("ctrl+z") {
		event.Handled = true
	}
	return nil
}

func main(cmd *cli.Command, o *Options, args []string) (rc int, err error) {
	opts = o
	err = parse_place()
	if err != nil {
		return 1, err
	}
	err = parse_z_index()
	if err != nil {
		return 1, err
	}
	err = parse_background()
	if err != nil {
		return 1, err
	}
	err = parse_mirror()
	if err != nil {
		return 1, err
	}
	stderr_is_tty = tty.IsTerminal(os.Stderr.Fd())
	if opts.PrintWindowSize {
		t, err := tty.OpenControllingTerm()
		if err != nil {
			return 1, fmt.Errorf("Failed to open controlling terminal with error: %w", err)
		}
		sz, err := t.GetSize()
		if err != nil {
			return 1, fmt.Errorf("Failed to query terminal using TIOCGWINSZ with error: %w", err)
		}
		fmt.Printf("%dx%d", sz.Xpixel, sz.Ypixel)
		return 0, nil
	}
	temp_files_to_delete = make([]string, 0, 8)
	shm_files_to_delete = make([]shm.MMap, 0, 8)
	lp, err = loop.New(loop.NoAlternateScreen, loop.NoRestoreColors, loop.NoMouseTracking)
	if err != nil {
		return
	}
	lp.OnInitialize = on_initialize
	lp.OnFinalize = on_finalize
	lp.OnEscapeCode = on_escape_code
	lp.OnWakeup = on_wakeup
	items, err := process_dirs(args...)
	if err != nil {
		return 1, err
	}
	if opts.Place != "" && len(items) > 1 {
		return 1, fmt.Errorf("The --place option can only be used with a single image, not %d", len(items))
	}
	files_channel = make(chan input_arg, len(items))
	for _, ia := range items {
		files_channel <- ia
	}
	num_of_items = len(items)
	output_channel = make(chan *image_data, 1)
	keep_going = &atomic.Bool{}

	err = lp.Run()
	keep_going.Store(false)
	if err != nil {
		return
	}
	ds := lp.DeathSignalName()
	if ds != "" {
		fmt.Println("Killed by signal: ", ds)
		lp.KillIfSignalled()
		return
	}
	if opts.Hold {
		fmt.Print("\r")
		if opts.Place != "" {
			fmt.Println()
		}
		tui.HoldTillEnter(false)
	}
	return 0, nil
}

func EntryPoint(parent *cli.Command) {
	create_cmd(parent, main)
}
