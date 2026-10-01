package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cehbz/multicam/internal/sony"
)

type options struct {
	body     string
	out      string
	endpoint string // used when SSDP or the device description fails
	ssdp     bool
	wait     bool // retry discovery/reachability for waitFor at start
	record   bool
	zoom     bool
	liveview time.Duration // per liveview session
	frames   int           // JPEG frames saved per session

	ssdpAddr     string
	ssdpListen   time.Duration
	waitFor      time.Duration
	poll         time.Duration // getEvent polling interval
	hold         time.Duration // recording length and zoom hold
	readyTimeout time.Duration // startRecMode / setShootMode settle limit
}

func defaultOptions() options {
	return options{
		out:          "captures",
		endpoint:     sony.DefaultEndpoint,
		ssdp:         true,
		liveview:     5 * time.Second,
		frames:       5,
		ssdpAddr:     sony.SSDPMulticast,
		ssdpListen:   3 * time.Second,
		waitFor:      30 * time.Second,
		poll:         500 * time.Millisecond,
		hold:         3 * time.Second,
		readyTimeout: 15 * time.Second,
	}
}

// Methods whose presence decides what the rig can do with a body.
var capabilityMethods = []string{
	"startRecMode", "setShootMode", "startMovieRec", "stopMovieRec", "actZoom", "setZoomSetting",
	"setExposureMode", "setShutterSpeed", "setFNumber", "setIsoSpeedRate", "setExposureCompensation",
	"setWhiteBalance", "setFocusMode", "actHalfPressShutter", "setTouchAFPosition", "actTrackingFocus",
	"setMovieQuality", "setMovieFileFormat", "setSteadyMode",
	"startLiveview", "startLiveviewWithSize", "setLiveviewFrameInfo",
}

// Exposure families whose getAvailable*/getSupported* results are quoted in the summary.
var exposureFamilies = []string{"ShutterSpeed", "FNumber", "IsoSpeedRate", "ExposureCompensation", "WhiteBalance", "FocusMode", "ExposureMode"}

var queryMethod = regexp.MustCompile(`^get(Available|Supported)[A-Z]`)

// recorder writes every artefact of a run into one directory, numbered in
// call order, and mirrors the summary to stdout.
type recorder struct {
	dir      string
	seq      int
	summary  *os.File
	failures []string
}

func newRecorder(o options) (*recorder, error) {
	body := strings.NewReplacer("/", "_", string(os.PathSeparator), "_").Replace(o.body)
	dir := filepath.Join(o.out, body, time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.Create(filepath.Join(dir, "summary.txt"))
	if err != nil {
		return nil, err
	}
	return &recorder{dir: dir, summary: f}, nil
}

// next reserves a sequence number shared by the files of one step.
func (r *recorder) next() string {
	r.seq++
	return fmt.Sprintf("%03d", r.seq)
}

func (r *recorder) write(name string, data []byte) {
	if err := os.WriteFile(filepath.Join(r.dir, name), data, 0o644); err != nil {
		r.logf("  ! write %s: %v", name, err)
	}
}

func (r *recorder) logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...) + "\n"
	io.WriteString(os.Stdout, line)
	r.summary.WriteString(line)
}

func (r *recorder) fail(step string, err error) {
	msg := fmt.Sprintf("%s: %v", step, err)
	r.failures = append(r.failures, msg)
	r.logf("  FAILED %s", msg)
}

func (r *recorder) close() error { return r.summary.Close() }

type probe struct {
	o    options
	rec  *recorder
	cam  *sony.Client
	http *http.Client

	supported    map[string][]string // method -> versions, from getMethodTypes
	available    []string            // latest getAvailableApiList
	eventVersion string
	results      map[string]string // method -> compact result, for the summary
}

func run(ctx context.Context, o options) error {
	rec, err := newRecorder(o)
	if err != nil {
		return err
	}
	defer rec.close()
	p := &probe{
		o: o, rec: rec,
		http:         &http.Client{Timeout: 10 * time.Second},
		supported:    map[string][]string{},
		eventVersion: "1.0",
		results:      map[string]string{},
	}
	rec.logf("sonyprobe body=%s started %s", o.body, time.Now().Format(time.RFC3339))
	rec.logf("flags: ssdp=%v wait=%v record=%v zoom=%v liveview=%v", o.ssdp, o.wait, o.record, o.zoom, o.liveview)

	p.cam = sony.NewClient(p.discover(ctx))
	p.initial(ctx)
	p.recMode(ctx)
	p.movieMode(ctx)
	p.queries(ctx)
	if o.record {
		p.recordMovie(ctx)
	}
	if o.zoom {
		p.zoomTest(ctx)
	}
	p.liveviews(ctx)
	p.capabilities()

	rec.logf("\n== failures (%d)", len(rec.failures))
	for _, f := range rec.failures {
		rec.logf("  %s", f)
	}
	rec.logf("captures in %s", rec.dir)
	return nil
}

// discover finds the camera endpoint by SSDP and the device description,
// falling back to o.endpoint. With -wait it retries until o.waitFor elapses.
func (p *probe) discover(ctx context.Context) string {
	p.rec.logf("\n== discovery")
	deadline := time.Now()
	if p.o.wait {
		deadline = deadline.Add(p.o.waitFor)
	}
	if p.o.ssdp {
		for attempt := 1; ctx.Err() == nil; attempt++ {
			ep, err := p.ssdpOnce(ctx, attempt)
			if err == nil {
				p.rec.logf("endpoint %s (from device description)", ep)
				return ep
			}
			p.rec.logf("  ssdp attempt %d: %v", attempt, err)
			if !time.Now().Before(deadline) {
				break
			}
			sleep(ctx, time.Second)
		}
		p.rec.fail("ssdp discovery", errors.New("no usable reply"))
	}
	p.rec.logf("endpoint %s (-endpoint fallback)", p.o.endpoint)
	for attempt := 1; ctx.Err() == nil; attempt++ {
		err := reachable(p.o.endpoint)
		if err == nil {
			p.rec.logf("  endpoint reachable (attempt %d)", attempt)
			break
		}
		p.rec.logf("  endpoint unreachable (attempt %d): %v", attempt, err)
		if !time.Now().Before(deadline) {
			p.rec.fail("endpoint reachability", err)
			break
		}
		sleep(ctx, time.Second)
	}
	return p.o.endpoint
}

func reachable(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "80")
	}
	c, err := net.DialTimeout("tcp", host, 2*time.Second)
	if err != nil {
		return err
	}
	return c.Close()
}

func (p *probe) ssdpOnce(ctx context.Context, attempt int) (string, error) {
	req, replies, err := sony.Search(ctx, p.o.ssdpAddr, p.o.ssdpListen)
	if attempt == 1 {
		p.rec.write(p.rec.next()+"-ssdp-request.txt", req)
	}
	if err != nil {
		return "", err
	}
	if len(replies) == 0 {
		return "", errors.New("no replies")
	}
	var loc string
	for i, r := range replies {
		p.rec.write(fmt.Sprintf("%s-ssdp-reply-%d.txt", p.rec.next(), i+1), r.Raw)
		resp, err := sony.ParseSSDPResponse(r.Raw)
		if err != nil {
			p.rec.logf("  reply %d from %s: %v", i+1, r.From, err)
			continue
		}
		p.rec.logf("  reply %d from %s: ST %s, LOCATION %s, SERVER %s", i+1, r.From, resp.Header.Get("ST"), resp.Location(), resp.Header.Get("Server"))
		if loc == "" && resp.Header.Get("ST") == sony.SearchTarget {
			loc = resp.Location()
		}
	}
	if loc == "" {
		return "", errors.New("no reply for " + sony.SearchTarget)
	}

	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, loc, nil)
	if err != nil {
		return "", err
	}
	resp, err := p.http.Do(hreq)
	if err != nil {
		return "", fmt.Errorf("fetch device description: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	p.rec.write(p.rec.next()+"-device-description.xml", body)
	if err != nil {
		return "", fmt.Errorf("read device description: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("device description: HTTP %s", resp.Status)
	}
	dd, err := sony.ParseDeviceDescription(body)
	if err != nil {
		return "", err
	}
	p.rec.logf("  device %q model %q API version %s", dd.FriendlyName, dd.ModelName, dd.APIVersion)
	for _, s := range dd.Services {
		p.rec.logf("  service %-14s %s", s.Type, s.Endpoint())
	}
	cam := dd.Service("camera")
	if cam == nil {
		return "", errors.New("device description lists no camera service")
	}
	return cam.Endpoint(), nil
}

// call performs one camera RPC, saving the raw exchange and a summary line.
func (p *probe) call(ctx context.Context, step, method, version string, params ...any) (*sony.Response, error) {
	ex, err := p.cam.Call(ctx, method, version, params...)
	n := p.rec.next()
	base := fmt.Sprintf("%s-%s-%s", n, step, method)
	p.rec.write(base+".request.json", ex.Request)
	if ex.Response != nil {
		p.rec.write(base+".response.json", ex.Response)
	}
	if err != nil {
		p.rec.write(base+".error.txt", []byte(err.Error()+"\n"))
		p.rec.fail(fmt.Sprintf("[%s] %s", n, method), err)
		return ex.Decoded, err
	}
	p.rec.logf("  [%s] %s%s ok", n, method, paramString(params))
	return ex.Decoded, nil
}

func paramString(params []any) string {
	if len(params) == 0 {
		return ""
	}
	var s []string
	for _, v := range params {
		s = append(s, fmt.Sprintf("%v", v))
	}
	return "(" + strings.Join(s, ", ") + ")"
}

func compact(raw []byte, max int) string {
	s := strings.Join(strings.Fields(string(raw)), " ")
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}

func (p *probe) has(method string) bool { return slices.Contains(p.available, method) }

func (p *probe) listAPIs(ctx context.Context, step string) {
	r, err := p.call(ctx, step, "getAvailableApiList", "1.0")
	if err != nil {
		return
	}
	l, err := r.StringList()
	if err != nil {
		p.rec.fail(step+" getAvailableApiList", err)
		return
	}
	p.available = l
}

func (p *probe) event(ctx context.Context, step string) *sony.Event {
	r, err := p.call(ctx, step, "getEvent", p.eventVersion, false)
	if err != nil && p.eventVersion != "1.0" {
		p.rec.logf("  getEvent %s failed; using 1.0 from now on", p.eventVersion)
		p.eventVersion = "1.0"
		r, err = p.call(ctx, step, "getEvent", p.eventVersion, false)
	}
	if err != nil {
		return nil
	}
	ev, err := r.Event()
	if err != nil {
		p.rec.fail(step+" getEvent decode", err)
		return nil
	}
	return ev
}

func (p *probe) initial(ctx context.Context) {
	p.rec.logf("\n== service info")
	versions := []string{"1.0"}
	if r, err := p.call(ctx, "info", "getVersions", "1.0"); err == nil {
		if v, err := r.StringList(); err == nil {
			versions = v
			p.rec.logf("  API versions: %s", strings.Join(v, ", "))
		}
	}
	for _, v := range versions {
		r, err := p.call(ctx, "info", "getMethodTypes", "1.0", v)
		if err != nil {
			continue
		}
		mts, err := r.MethodTypes()
		if err != nil {
			p.rec.fail("getMethodTypes decode", err)
			continue
		}
		for _, m := range mts {
			if !slices.Contains(p.supported[m.Name], m.Version) {
				p.supported[m.Name] = append(p.supported[m.Name], m.Version)
			}
		}
		p.rec.logf("  getMethodTypes(%s): %d methods", v, len(mts))
	}
	for _, v := range p.supported["getEvent"] {
		if versionLess(p.eventVersion, v) {
			p.eventVersion = v
		}
	}
	p.rec.logf("  using getEvent version %s", p.eventVersion)
	if r, err := p.call(ctx, "info", "getApplicationInfo", "1.0"); err == nil {
		p.rec.logf("  application info: %s", compact(r.Result, 200))
	}
	p.listAPIs(ctx, "info")
	p.rec.logf("  available APIs (%d): %s", len(p.available), strings.Join(p.available, " "))
	if ev := p.event(ctx, "info"); ev != nil {
		p.rec.logf("  cameraStatus %q shootMode %q", ev.CameraStatus, ev.ShootMode)
	}
}

func versionLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}

// settle polls getEvent and getAvailableApiList until ready reports true or
// the timeout passes.
func (p *probe) settle(ctx context.Context, step string, ready func(*sony.Event) bool) {
	start := time.Now()
	var statuses []string
	for ctx.Err() == nil {
		ev := p.event(ctx, step)
		p.listAPIs(ctx, step)
		status := ""
		if ev != nil {
			status = ev.CameraStatus
		}
		if len(statuses) == 0 || statuses[len(statuses)-1] != status {
			statuses = append(statuses, status)
		}
		if ready(ev) {
			p.rec.logf("  camera ready after %.1f s (cameraStatus %s)", time.Since(start).Seconds(), strings.Join(statuses, " -> "))
			return
		}
		if time.Since(start) > p.o.readyTimeout {
			p.rec.fail(step+" settle", fmt.Errorf("not ready after %v (cameraStatus %s)", p.o.readyTimeout, strings.Join(statuses, " -> ")))
			return
		}
		sleep(ctx, p.o.poll)
	}
}

func idle(ev *sony.Event) bool {
	return ev == nil || ev.CameraStatus == "" || ev.CameraStatus == "IDLE"
}

func (p *probe) recMode(ctx context.Context) {
	p.rec.logf("\n== rec mode")
	if !p.has("startRecMode") {
		p.rec.logf("  startRecMode not listed; skipped")
		return
	}
	if _, err := p.call(ctx, "recmode", "startRecMode", "1.0"); err != nil {
		return
	}
	p.settle(ctx, "recmode", func(ev *sony.Event) bool {
		return idle(ev) && (p.has("setShootMode") || p.has("startLiveview") || p.has("startMovieRec"))
	})
	p.rec.logf("  available APIs (%d): %s", len(p.available), strings.Join(p.available, " "))
}

func (p *probe) movieMode(ctx context.Context) {
	p.rec.logf("\n== movie mode")
	if !p.has("setShootMode") {
		p.rec.logf("  setShootMode not listed; skipped")
	} else if _, err := p.call(ctx, "movie", "setShootMode", "1.0", "movie"); err == nil {
		p.settle(ctx, "movie", func(ev *sony.Event) bool {
			return idle(ev) && (p.has("startMovieRec") || (ev != nil && ev.ShootMode == "movie"))
		})
	}
	p.listAPIs(ctx, "movie")
	p.rec.logf("  available APIs (%d): %s", len(p.available), strings.Join(p.available, " "))
	if ev := p.event(ctx, "movie"); ev != nil {
		p.rec.logf("  cameraStatus %q shootMode %q", ev.CameraStatus, ev.ShootMode)
	}
}

// queries calls every listed getAvailable*/getSupported* method; all are
// read-only.
func (p *probe) queries(ctx context.Context) {
	p.rec.logf("\n== parameter queries")
	var names []string
	for _, m := range p.available {
		if queryMethod.MatchString(m) {
			names = append(names, m)
		}
	}
	slices.Sort(names)
	for _, m := range names {
		if r, err := p.call(ctx, "query", m, "1.0"); err == nil {
			p.results[m] = compact(r.Result, 400)
		}
	}
	p.rec.logf("\n-- exposure families")
	for _, fam := range exposureFamilies {
		for _, kind := range []string{"Available", "Supported"} {
			m := "get" + kind + fam
			switch v, ok := p.results[m]; {
			case ok:
				p.rec.logf("  %-35s %s", m, v)
			case p.has(m):
				p.rec.logf("  %-35s (failed)", m)
			default:
				p.rec.logf("  %-35s not listed", m)
			}
		}
	}
}

func (p *probe) recordMovie(ctx context.Context) {
	p.rec.logf("\n== record test")
	if !p.has("startMovieRec") {
		p.rec.logf("  startMovieRec not listed; trying anyway")
	}
	var statuses []string
	track := func() {
		if ev := p.event(ctx, "record"); ev != nil && (len(statuses) == 0 || statuses[len(statuses)-1] != ev.CameraStatus) {
			statuses = append(statuses, ev.CameraStatus)
		}
	}
	track()
	if _, err := p.call(ctx, "record", "startMovieRec", "1.0"); err != nil {
		return
	}
	start := time.Now()
	for time.Since(start) < p.o.hold && ctx.Err() == nil {
		sleep(ctx, p.o.poll)
		track()
	}
	if r, err := p.call(ctx, "record", "stopMovieRec", "1.0"); err == nil {
		p.rec.logf("  stopMovieRec result %s", compact(r.Result, 200))
	}
	stop := time.Now()
	for ctx.Err() == nil {
		track()
		if (len(statuses) > 0 && statuses[len(statuses)-1] == "IDLE") || time.Since(stop) > p.o.readyTimeout {
			break
		}
		sleep(ctx, p.o.poll)
	}
	p.rec.logf("  cameraStatus %s", strings.Join(statuses, " -> "))
}

func (p *probe) zoomTest(ctx context.Context) {
	p.rec.logf("\n== zoom test")
	pos := func() string {
		if ev := p.event(ctx, "zoom"); ev != nil && ev.ZoomPosition != nil {
			return strconv.Itoa(*ev.ZoomPosition)
		}
		return "?"
	}
	line := "zoomPosition " + pos()
	for _, dir := range []string{"in", "out"} {
		if _, err := p.call(ctx, "zoom", "actZoom", "1.0", dir, "start"); err != nil {
			line += " -> " + dir + " failed"
			continue
		}
		sleep(ctx, p.o.hold/3)
		p.call(ctx, "zoom", "actZoom", "1.0", dir, "stop")
		line += " -> " + dir + " " + pos()
	}
	p.rec.logf("  %s", line)
}

func (p *probe) liveviews(ctx context.Context) {
	p.rec.logf("\n== liveview")
	if !p.has("startLiveview") {
		p.rec.logf("  startLiveview not listed; trying anyway")
	}
	if r, err := p.call(ctx, "liveview", "startLiveview", "1.0"); err == nil {
		p.session(ctx, "default", r)
	}
	if !p.has("startLiveviewWithSize") {
		return
	}
	var sizes []string
	for _, m := range []string{"getSupportedLiveviewSize", "getAvailableLiveviewSize"} {
		if len(sizes) > 0 {
			break
		}
		r, err := p.call(ctx, "liveview", m, "1.0")
		if err != nil {
			continue
		}
		if l, err := r.StringList(); err == nil {
			sizes = l
		}
	}
	for _, size := range sizes {
		if r, err := p.call(ctx, "liveview", "startLiveviewWithSize", "1.0", size); err == nil {
			p.session(ctx, "size-"+size, r)
		}
	}
}

// session reads one liveview stream for o.liveview, then stops liveview.
func (p *probe) session(ctx context.Context, label string, start *sony.Response) {
	defer p.call(ctx, "liveview", "stopLiveview", "1.0")
	u, err := start.FirstString()
	if err != nil {
		p.rec.fail("liveview "+label, err)
		return
	}
	st, err := p.stream(ctx, label, u)
	if err != nil {
		p.rec.fail("liveview "+label, err)
	}
	if st == nil {
		return
	}
	p.rec.logf("  %s", st.describe(strings.Replace(label, "size-", "size ", 1), u))
}

type streamStats struct {
	packets, jpegs, infos int
	bytes, jpegBytes      int64
	elapsed               time.Duration
	firstTS, lastTS       uint32
	dims                  map[string]int
	end                   string
}

func (s *streamStats) describe(label, u string) string {
	secs := s.elapsed.Seconds()
	var b strings.Builder
	fmt.Fprintf(&b, "liveview %s (%s): %d JPEG + %d frame-info packets in %.2f s", label, u, s.jpegs, s.infos, secs)
	if secs > 0 {
		fmt.Fprintf(&b, "; %.1f fps wall", float64(s.jpegs)/secs)
		fmt.Fprintf(&b, "; %.0f KB/s", float64(s.bytes)/secs/1024)
	}
	if s.jpegs > 1 && s.lastTS > s.firstTS {
		fmt.Fprintf(&b, "; %.1f fps camera clock", float64(s.jpegs-1)/(float64(s.lastTS-s.firstTS)/1000))
	}
	if s.jpegs > 0 {
		fmt.Fprintf(&b, "; mean JPEG %.1f KB", float64(s.jpegBytes)/float64(s.jpegs)/1024)
	}
	var dims []string
	for d, n := range s.dims {
		dims = append(dims, fmt.Sprintf("%s x%d", d, n))
	}
	slices.Sort(dims)
	fmt.Fprintf(&b, "; dimensions %s; ended: %s", strings.Join(dims, ", "), s.end)
	return b.String()
}

// limitedBuffer keeps the first max bytes written to it.
type limitedBuffer struct {
	bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(b []byte) (int, error) {
	if room := l.max - l.Len(); room > 0 {
		l.Buffer.Write(b[:min(room, len(b))])
	}
	return len(b), nil
}

func (p *probe) stream(ctx context.Context, label, u string) (*streamStats, error) {
	n := p.rec.next()
	base := fmt.Sprintf("%s-liveview-%s", n, label)
	sctx, cancel := context.WithTimeout(ctx, p.o.liveview)
	defer cancel()
	req, err := http.NewRequestWithContext(sctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var hdr bytes.Buffer
	fmt.Fprintf(&hdr, "%s %s\n", resp.Proto, resp.Status)
	resp.Header.Write(&hdr)
	p.rec.write(base+".headers.txt", hdr.Bytes())
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}

	raw := &limitedBuffer{max: 2 << 20}
	counted := &countingReader{r: io.TeeReader(resp.Body, raw)}
	lv := sony.NewLiveviewReader(counted)
	st := &streamStats{dims: map[string]int{}}
	began := time.Now()
	defer func() { p.rec.write(base+".stream.bin", raw.Bytes()) }()
	for {
		f, err := lv.Next()
		st.elapsed = time.Since(began)
		st.bytes = counted.n
		if err != nil {
			if sctx.Err() != nil && ctx.Err() == nil {
				st.end = "read window elapsed"
				return st, nil
			}
			st.end = err.Error()
			return st, err
		}
		st.packets++
		if f.Type == sony.PayloadFrameInfo {
			st.infos++
			continue
		}
		st.jpegs++
		st.jpegBytes += int64(len(f.Data))
		if st.jpegs == 1 {
			st.firstTS = f.Timestamp
		}
		st.lastTS = f.Timestamp
		if cfg, _, err := image.DecodeConfig(bytes.NewReader(f.Data)); err == nil {
			st.dims[fmt.Sprintf("%dx%d", cfg.Width, cfg.Height)]++
		} else {
			st.dims["undecodable"]++
		}
		if st.jpegs <= p.o.frames {
			p.rec.write(fmt.Sprintf("%s-frame-%d.jpg", base, st.jpegs), f.Data)
		}
	}
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n += int64(n)
	return n, err
}

func (p *probe) capabilities() {
	p.rec.logf("\n== capabilities (supported = getMethodTypes; available = last getAvailableApiList)")
	for _, m := range capabilityMethods {
		sup := "-"
		if v := p.supported[m]; len(v) > 0 {
			sup = strings.Join(v, ",")
		}
		avail := "no"
		if p.has(m) {
			avail = "yes"
		}
		p.rec.logf("  %-26s supported %-8s available %s", m, sup, avail)
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
