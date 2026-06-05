package quickjs_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/colespringer/waxseal/internal/jsruntime"
)

// DOM fidelity checks run offline in QuickJS. They cover prototype chains,
// createElement typing, native-looking Function.prototype.toString,
// Request/EventTarget, canvas/media probes, and timezone/Date coherence. These
// assert local browser-surface invariants; the live integrity-token outcome
// still needs network and real BotGuard.

// evalTrue asserts a JS boolean expression evaluates to true under the loaded
// bg_bundle (default BrowserProfile applied).
func evalTrue(t *testing.T, rt jsruntime.Runtime, name, expr string) {
	t.Helper()
	out, err := rt.Eval(context.Background(), "Boolean("+expr+")")
	if err != nil {
		t.Errorf("%s: eval error: %v", name, err)
		return
	}
	if string(out) != "true" {
		// Re-eval raw for a useful failure message.
		raw, _ := rt.Eval(context.Background(), "String("+expr+")")
		t.Errorf("%s: got false, want true (value=%s)", name, raw)
	}
}

func evalString(t *testing.T, rt jsruntime.Runtime, expr string) string {
	t.Helper()
	out, err := rt.Eval(context.Background(), expr)
	if err != nil {
		t.Fatalf("eval %q: %v", expr, err)
	}
	var s string
	if err := json.Unmarshal(out, &s); err != nil {
		t.Fatalf("eval %q: not a string: %s", expr, out)
	}
	return s
}

// The canonical chain HTMLDivElement -> HTMLElement -> Element -> Node ->
// EventTarget, and createElement instances satisfy instanceof at every level.
func TestDOMPrototypeChain(t *testing.T) {
	rt := newBundledRT(t)
	cases := map[string]string{
		"proto: HTMLDivElement->HTMLElement": "Object.getPrototypeOf(HTMLDivElement.prototype) === HTMLElement.prototype",
		"proto: HTMLElement->Element":        "Object.getPrototypeOf(HTMLElement.prototype) === Element.prototype",
		"proto: Element->Node":               "Object.getPrototypeOf(Element.prototype) === Node.prototype",
		"proto: Node->EventTarget":           "Object.getPrototypeOf(Node.prototype) === EventTarget.prototype",
		"div instanceof HTMLDivElement":      "document.createElement('div') instanceof HTMLDivElement",
		"div instanceof HTMLElement":         "document.createElement('div') instanceof HTMLElement",
		"div instanceof Element":             "document.createElement('div') instanceof Element",
		"div instanceof Node":                "document.createElement('div') instanceof Node",
		"div instanceof EventTarget":         "document.createElement('div') instanceof EventTarget",
		"div.tagName":                        "document.createElement('div').tagName === 'DIV'",
		"div.appendChild works":              "(() => { const d=document.createElement('div'); const s=document.createElement('span'); d.appendChild(s); return d.childNodes[0]===s && s.parentNode===d; })()",
	}
	for name, expr := range cases {
		evalTrue(t, rt, name, expr)
	}
}

// Window geometry must match browser types. Live BotGuard probes have read
// window.screenY, and modern browsers expose the legacy screenLeft/screenTop
// aliases alongside inner/outer dimensions.
func TestWindowGeometry(t *testing.T) {
	rt := newBundledRT(t)
	for _, prop := range []string{
		"screenX", "screenY", "screenLeft", "screenTop",
		"innerWidth", "innerHeight", "outerWidth", "outerHeight", "devicePixelRatio",
	} {
		evalTrue(t, rt, prop+" is a number", "typeof window."+prop+" === 'number'")
	}
	evalTrue(t, rt, "screenLeft mirrors screenX", "window.screenLeft === window.screenX")
	evalTrue(t, rt, "screenTop mirrors screenY", "window.screenTop === window.screenY")
}

// createElement maps tags to the correct interface (and the media/SVG sub-chains
// hold), so the whole instanceof battery agrees with createElement.
func TestCreateElementTyping(t *testing.T) {
	rt := newBundledRT(t)
	cases := map[string]string{
		"canvas":             "document.createElement('canvas') instanceof HTMLCanvasElement",
		"video->media":       "document.createElement('video') instanceof HTMLVideoElement && document.createElement('video') instanceof HTMLMediaElement",
		"audio->media":       "document.createElement('audio') instanceof HTMLAudioElement && document.createElement('audio') instanceof HTMLMediaElement",
		"anchor":             "document.createElement('a') instanceof HTMLAnchorElement",
		"img":                "document.createElement('img') instanceof HTMLImageElement",
		"script":             "document.createElement('script') instanceof HTMLScriptElement",
		"iframe":             "document.createElement('iframe') instanceof HTMLIFrameElement",
		"unknown->Unknown":   "document.createElement('blink-xyz') instanceof HTMLUnknownElement",
		"svg via NS":         "document.createElementNS('http://www.w3.org/2000/svg','svg') instanceof SVGSVGElement",
		"svg->graphics->svg": "document.createElementNS('http://www.w3.org/2000/svg','svg') instanceof SVGGraphicsElement && document.createElementNS('http://www.w3.org/2000/svg','svg') instanceof SVGElement && document.createElementNS('http://www.w3.org/2000/svg','svg') instanceof Element",
		"svg path":           "document.createElementNS('http://www.w3.org/2000/svg','path') instanceof SVGPathElement",
	}
	for name, expr := range cases {
		evalTrue(t, rt, name, expr)
	}
}

// The standard HTML element battery is present and createElement-coherent. Live
// probes have included window.HTMLMeterElement, so every tag must mint an
// instance of its declared interface and every interface must be a real function
// on window.
func TestFullElementBattery(t *testing.T) {
	rt := newBundledRT(t)
	// tag -> interface name (a representative cross-section incl. the special
	// parents and the probed HTMLMeterElement).
	tagIface := map[string]string{
		"br": "HTMLBRElement", "hr": "HTMLHRElement", "pre": "HTMLPreElement",
		"q": "HTMLQuoteElement", "blockquote": "HTMLQuoteElement", "meter": "HTMLMeterElement",
		"progress": "HTMLProgressElement", "output": "HTMLOutputElement", "details": "HTMLDetailsElement",
		"dialog": "HTMLDialogElement", "fieldset": "HTMLFieldSetElement", "legend": "HTMLLegendElement",
		"h1": "HTMLHeadingElement", "h6": "HTMLHeadingElement", "ins": "HTMLModElement",
		"del": "HTMLModElement", "td": "HTMLTableCellElement", "th": "HTMLTableCellElement",
		"tr": "HTMLTableRowElement", "thead": "HTMLTableSectionElement", "col": "HTMLTableColElement",
		"slot": "HTMLSlotElement", "time": "HTMLTimeElement", "track": "HTMLTrackElement",
		"map": "HTMLMapElement", "area": "HTMLAreaElement", "object": "HTMLObjectElement",
		"embed": "HTMLEmbedElement", "menu": "HTMLMenuElement", "data": "HTMLDataElement",
		"datalist": "HTMLDataListElement", "optgroup": "HTMLOptGroupElement", "caption": "HTMLTableCaptionElement",
	}
	for tag, iface := range tagIface {
		evalTrue(t, rt, "createElement("+tag+") instanceof "+iface,
			"typeof window."+iface+" === 'function' && document.createElement('"+tag+"') instanceof "+iface+
				" && document.createElement('"+tag+"') instanceof HTMLElement")
	}
	// A live-probed interface must no longer be undefined.
	evalTrue(t, rt, "HTMLMeterElement defined", "typeof HTMLMeterElement === 'function'")
}

// Native Function.prototype.toString: every DOM constructor/method/accessor and
// shim host function reports `[native code]`.
func TestNativeToString(t *testing.T) {
	rt := newBundledRT(t)

	exact := map[string]string{
		"document.createElement.toString()":      "function createElement() { [native code] }",
		"HTMLDivElement.toString()":              "function HTMLDivElement() { [native code] }",
		"Function.prototype.toString.toString()": "function toString() { [native code] }",
		"setTimeout.toString()":                  "function setTimeout() { [native code] }",
		"Object.getOwnPropertyDescriptor(Element.prototype,'tagName').get.toString()": "function get tagName() { [native code] }",
	}
	for expr, want := range exact {
		if got := evalString(t, rt, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}

	// Called as Function.prototype.toString.call(fn) too (BotGuard's usual form).
	containsNative := []string{
		"Function.prototype.toString.call(document.createElement)",
		"document.createElement('canvas').getContext.toString()",
		"document.addEventListener.toString()",
		"navigator.javaEnabled.toString()",
		"Math.random.toString()",
		"crypto.getRandomValues.toString()",
		"EventTarget.prototype.addEventListener.toString()",
		"Date.prototype.getTimezoneOffset.toString()",
		"WebGLRenderingContext.prototype.getParameter.toString()",
	}
	for _, expr := range containsNative {
		if got := evalString(t, rt, expr); !strings.Contains(got, "[native code]") {
			t.Errorf("%s = %q, want it to contain [native code]", expr, got)
		}
	}

	// Do not globally fake toString: page/user functions must still report their
	// real source. bgutils-js and challenge JS are page script, not native.
	if got := evalString(t, rt, "(function pageFn(){ return 41 + 1; }).toString()"); strings.Contains(got, "[native code]") {
		t.Errorf("non-native fn falsely reports native: %q", got)
	}
}

// Direct construction of a DOM interface throws "Illegal constructor"; the few
// genuinely-constructable ones (EventTarget, Event) do not.
func TestIllegalConstructor(t *testing.T) {
	rt := newBundledRT(t)
	throws := []string{"HTMLDivElement", "HTMLElement", "Element", "Node", "Document", "HTMLCanvasElement", "VideoTrack"}
	for _, ctor := range throws {
		expr := "(() => { try { new " + ctor + "(); return false; } catch(e){ return e instanceof TypeError && /Illegal constructor/.test(e.message); } })()"
		evalTrue(t, rt, "new "+ctor+" throws", expr)
	}
	evalTrue(t, rt, "new EventTarget() ok", "(() => { try { return new EventTarget() instanceof EventTarget; } catch(e){ return false; } })()")
	evalTrue(t, rt, "new Event() ok", "(() => { const e = new Event('x'); return e instanceof Event && e.type === 'x'; })()")
}

// Date timezone coherence: getTimezoneOffset matches the profile, and the local
// getters are derived from UTC+offset so the whole Date surface agrees. Default
// profile is America/Phoenix, UTC-7 year-round.
func TestDateTimezoneCoherence(t *testing.T) {
	rt := newBundledRT(t)
	cases := map[string]string{
		"getTimezoneOffset == 420": "new Date().getTimezoneOffset() === 420",
		// noon UTC on 2021-01-01 is 05:00 local at UTC-7.
		"local hours shifted":  "new Date(Date.UTC(2021,0,1,12,0,0)).getHours() === 5",
		"utc hours intact":     "new Date(Date.UTC(2021,0,1,12,0,0)).getUTCHours() === 12",
		"date fields coherent": "(() => { const d=new Date(Date.UTC(2021,0,1,12,0,0)); return d.getFullYear()===2021 && d.getMonth()===0 && d.getDate()===1; })()",
		// Crossing midnight backward: 02:00 UTC -> 19:00 previous day local at UTC-7.
		"midnight rollback": "(() => { const d=new Date(Date.UTC(2021,0,2,2,0,0)); return d.getHours()===19 && d.getDate()===1; })()",
		// Non-DST: summer and winter dates report the same offset. New York would
		// differ, which is the incoherence this default avoids.
		"non-DST summer==winter": "new Date(Date.UTC(2021,6,1,12,0,0)).getTimezoneOffset() === new Date(Date.UTC(2021,0,1,12,0,0)).getTimezoneOffset()",
		"Intl timezone name":     "Intl.DateTimeFormat().resolvedOptions().timeZone === 'America/Phoenix'",
	}
	for name, expr := range cases {
		evalTrue(t, rt, name, expr)
	}

	// A different profile re-pins the offset coherently (Europe/Berlin, +60).
	if _, err := rt.Call(context.Background(), "__wxApplyProfile", map[string]any{
		"timezone": "Europe/Berlin", "utcOffsetMinutes": 60,
	}); err != nil {
		t.Fatalf("apply Berlin profile: %v", err)
	}
	evalTrue(t, rt, "Berlin offset", "new Date().getTimezoneOffset() === -60")
	evalTrue(t, rt, "Berlin local hours", "new Date(Date.UTC(2021,0,1,12,0,0)).getHours() === 13")
}

// Canvas + WebGL: an instanceof-correct surface with plausible WebGL parameters
// and a stable data URL.
func TestCanvasAndWebGL(t *testing.T) {
	rt := newBundledRT(t)
	cases := map[string]string{
		"2d context type":   "document.createElement('canvas').getContext('2d') instanceof CanvasRenderingContext2D",
		"2d canvas backref": "(() => { const c=document.createElement('canvas'); return c.getContext('2d').canvas === c; })()",
		"2d methods":        "(() => { const x=document.createElement('canvas').getContext('2d'); return typeof x.fillRect==='function' && typeof x.fillText==='function' && typeof x.getImageData==='function'; })()",
		"measureText":       "document.createElement('canvas').getContext('2d').measureText('hello').width > 0",
		"getImageData":      "(() => { const d=document.createElement('canvas').getContext('2d').getImageData(0,0,2,2); return d instanceof ImageData && d.data.length===16; })()",
		"toDataURL png":     "document.createElement('canvas').toDataURL().indexOf('data:image/png') === 0",
		"webgl type":        "document.createElement('canvas').getContext('webgl') instanceof WebGLRenderingContext",
		"webgl2 type":       "document.createElement('canvas').getContext('webgl2') instanceof WebGL2RenderingContext",
		"webgl vendor":      "document.createElement('canvas').getContext('webgl').getParameter(0x1F00) === 'WebKit'",
		"webgl unmasked":    "(() => { const gl=document.createElement('canvas').getContext('webgl'); const ext=gl.getExtension('WEBGL_debug_renderer_info'); return ext!=null && gl.getParameter(ext.UNMASKED_RENDERER_WEBGL).indexOf('ANGLE')===0; })()",
		"webgl extensions":  "document.createElement('canvas').getContext('webgl').getSupportedExtensions().indexOf('WEBGL_debug_renderer_info') >= 0",
	}
	for name, expr := range cases {
		evalTrue(t, rt, name, expr)
	}
}

// The broad platform-interface battery is present and native-looking. Live
// probes have included IDBVersionChangeEvent, MediaRecorderErrorEvent,
// StylePropertyMap, and CanvasCaptureMediaStreamTrack. Events are real Event
// subclasses; the legacy element factories (Image/Audio/Option) stay
// createElement-coherent.
func TestPlatformBattery(t *testing.T) {
	rt := newBundledRT(t)
	present := []string{
		"IDBVersionChangeEvent", "MediaRecorderErrorEvent", "StylePropertyMap", // earlier live-named gaps
		"CanvasCaptureMediaStreamTrack", "MediaStreamTrack", // CanvasCapture... was the latest live probe
		"MediaRecorder", "MediaSource", "AudioContext", "AnalyserNode", "OscillatorNode",
		"IDBDatabase", "IDBKeyRange", "CSSStyleSheet", "WebGLBuffer", "MutationObserver",
		"ResizeObserver", "IntersectionObserver", "ReadableStream", "XMLHttpRequest",
		"DOMException", "Range", "Notification", "RTCPeerConnection", "Animation", "FileReader",
	}
	for _, name := range present {
		evalTrue(t, rt, name+" present+native",
			"typeof window."+name+" === 'function' && Function.prototype.toString.call(window."+name+").indexOf('[native code]') >= 0")
	}
	// Event-family interfaces are constructable and chain to Event.
	evalTrue(t, rt, "IDBVersionChangeEvent chains Event", "new IDBVersionChangeEvent('versionchange') instanceof Event")
	evalTrue(t, rt, "AnimationEvent chains Event", "new AnimationEvent('x') instanceof Event")
	// Legacy element factories stay instanceof-coherent.
	evalTrue(t, rt, "new Image()", "new Image() instanceof HTMLImageElement && new Image(2,3).width === 2")
	evalTrue(t, rt, "new Audio()", "new Audio() instanceof HTMLAudioElement")
	evalTrue(t, rt, "new Option()", "new Option() instanceof HTMLOptionElement")
	// The battery must not overwrite the more specific chains defined above.
	evalTrue(t, rt, "MouseEvent chain intact", "new MouseEvent('click') instanceof UIEvent && new MouseEvent('click') instanceof Event")
	evalTrue(t, rt, "HTMLDivElement intact", "document.createElement('div') instanceof HTMLDivElement")

	// Where a global singleton exists, it must be instanceof its interface.
	coherence := map[string]string{
		"crypto instanceof Crypto":           "crypto instanceof Crypto",
		"crypto.subtle instanceof Subtle":    "crypto.subtle instanceof SubtleCrypto",
		"performance instanceof Performance": "performance instanceof Performance",
		"performance instanceof EventTarget": "performance instanceof EventTarget",
		"performance.now works":              "typeof performance.now() === 'number'",
		"localStorage instanceof Storage":    "localStorage instanceof Storage",
		"history instanceof History":         "history instanceof History",
		"navigator.plugins is PluginArray":   "navigator.plugins instanceof PluginArray",
	}
	for name, expr := range coherence {
		evalTrue(t, rt, name, expr)
	}

	// SVG element interfaces: present, chained, and createElementNS-coherent (the
	// trap named window.SVGSetElement). Value types are presence-only.
	svgNS := "'http://www.w3.org/2000/svg'"
	svg := map[string]string{
		"SVGSetElement present":    "typeof SVGSetElement === 'function'",
		"SVGAngle present":         "typeof SVGAngle === 'function'",
		"SVGPreserveAspectRatio":   "typeof SVGPreserveAspectRatio === 'function'",
		"createNS set coherent":    "document.createElementNS(" + svgNS + ",'set') instanceof SVGSetElement && document.createElementNS(" + svgNS + ",'set') instanceof SVGElement",
		"createNS circle coherent": "document.createElementNS(" + svgNS + ",'circle') instanceof SVGCircleElement && document.createElementNS(" + svgNS + ",'circle') instanceof SVGGeometryElement",
		"FileSystem present":       "typeof FileSystem === 'function'",
		"ManagedSourceBuffer":      "typeof ManagedSourceBuffer === 'function'",
	}
	for name, expr := range svg {
		evalTrue(t, rt, name, expr)
	}
}

// APIs named by the Proxy discovery trap are present and typed.
func TestPhase0ProbedInterfaces(t *testing.T) {
	rt := newBundledRT(t)
	cases := map[string]string{
		"SVGViewSpec":                  "typeof SVGViewSpec === 'function'",
		"VideoTrack":                   "typeof VideoTrack === 'function'",
		"Request":                      "typeof Request === 'function'",
		"window.length":                "window.length === 0",
		"HTMLVideoElement videoTracks": "document.createElement('video').videoTracks instanceof VideoTrackList",
		"media canPlayType":            "document.createElement('video').canPlayType('video/mp4') === 'probably'",
	}
	for name, expr := range cases {
		evalTrue(t, rt, name, expr)
	}
}

// The browser-object singletons are real instances (so `navigator instanceof
// Navigator` etc.), and the identity reads coherently from the profile.
func TestBrowserObjectInstances(t *testing.T) {
	rt := newBundledRT(t)
	cases := map[string]string{
		"navigator instanceof Navigator":   "navigator instanceof Navigator",
		"screen instanceof Screen":         "screen instanceof Screen",
		"location instanceof Location":     "location instanceof Location",
		"window instanceof Window":         "window instanceof Window",
		"window instanceof EventTarget":    "window instanceof EventTarget",
		"UA is Chrome":                     "/Chrome\\/\\d+/.test(navigator.userAgent)",
		"platform Win32":                   "navigator.platform === 'Win32'",
		"webdriver false":                  "navigator.webdriver === false",
		"languages frozen array":           "Array.isArray(navigator.languages) && Object.isFrozen(navigator.languages)",
		"userAgentData brands":             "navigator.userAgentData.brands.some(b => b.brand.indexOf('Chrom') >= 0)",
		"screen size":                      "screen.width === 1920 && screen.height === 1080",
		"document is HTMLDocument":         "document instanceof HTMLDocument && document instanceof Document",
		"document.body is HTMLBodyElement": "document.body instanceof HTMLBodyElement",
	}
	for name, expr := range cases {
		evalTrue(t, rt, name, expr)
	}
}

// Fetch interfaces behave (feature-detected by some probes; behavior-real).
func TestFetchInterfaces(t *testing.T) {
	rt := newBundledRT(t)
	cases := map[string]string{
		"Request method upper":   "new Request('https://x/', {method:'post'}).method === 'POST'",
		"Request headers type":   "new Request('u').headers instanceof Headers",
		"Headers get/set":        "(() => { const h=new Headers(); h.set('X-A','1'); return h.get('x-a')==='1' && h.has('X-A'); })()",
		"Response ok":            "new Response('body',{status:204}).ok === true",
		"AbortController signal": "(() => { const c=new AbortController(); const a=c.signal.aborted; c.abort(); return a===false && c.signal.aborted===true; })()",
		"URLSearchParams":        "new URLSearchParams('a=1&b=2').get('b') === '2'",
	}
	for name, expr := range cases {
		evalTrue(t, rt, name, expr)
	}
}
