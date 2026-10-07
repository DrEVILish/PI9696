package main

import (
	"encoding/json"
	"html/template"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

// The dashboard's timecode controls: a Timecode settings pane, the chase
// arm button under the deck, and the live readouts that ride the meter
// push (tcMeterView).

func tcSourceSelect() selectView {
	return settingSelect("tcsource", "/api/settings/timecode-source", "Timecode Source", "", func() optionsView {
		mutex.Lock()
		defer mutex.Unlock()
		return optionsView{Options: []string{"Off", "LTC (TIMECODE channel)", "MTC (RTP-MIDI)"}, Idx: tcSourceIdx}
	})
}

func tcRecordSelect() selectView {
	return settingSelect("tcrecord", "/api/settings/timecode-record", "Record Timecode", "", func() optionsView {
		mutex.Lock()
		defer mutex.Unlock()
		return optionsView{Options: []string{"Off", "As metadata", "As audio (+1 channel) and metadata"}, Idx: tcRecordIdx}
	})
}

func tcRateSelect() selectView {
	return settingSelect("tcrate", "/api/settings/timecode-rate", "Timecode Rate", "", func() optionsView {
		mutex.Lock()
		defer mutex.Unlock()
		names := make([]string, len(tcRates))
		for i, r := range tcRates {
			names[i] = r.Name + " fps"
		}
		return optionsView{Options: names, Idx: tcRateIdx}
	})
}

func tcOutputSwitch() switchView {
	mutex.Lock()
	defer mutex.Unlock()
	return switchView{Id: "tcoutput", Post: "/api/settings/timecode-output", InputId: "tcOutputToggle", Label: "Timecode Output",
		Readout: readout("ON", "OFF"), Hint: "LTC on TX TIMECODE, MTC to RTP-MIDI peers", Enabled: tcOutputOn}
}

var tcPeerFragmentTmpl = template.Must(template.New("tcpeer").Parse(`<div id="tcpeer" class="setting-cell">
<div class="field-row">
<form hx-post="/api/settings/timecode-peer" hx-target="#tcpeer" hx-swap="outerHTML">
<label class="label" for="tcPeerInput">MTC Peer</label>
<div class="input-group">
<input id="tcPeerInput" class="input" name="peer" type="text" value="{{.Peer}}" maxlength="64" placeholder="host or host:port" title="RTP-MIDI session to invite (port 5004 by default); peers can also invite this unit">
<button type="submit" class="btn btn-secondary">Save</button>
</div>
<span class="hint field-hint">{{if .Err}}<span class="err">{{.Err}}</span>{{else}}{{.Peers}}{{end}}</span>
</form>
</div>
</div>`))

type tcPeerView struct{ Peer, Peers, Err string }

func currentTCPeerView() tcPeerView {
	mutex.Lock()
	peer := tcMTCPeer
	mutex.Unlock()
	peers := tcPeersText()
	if peers == "" {
		peers = "RTP-MIDI session off (needs MTC source or output)"
	}
	return tcPeerView{Peer: peer, Peers: peers}
}

func tcRestartSwitch() switchView {
	mutex.Lock()
	defer mutex.Unlock()
	return switchView{Id: "tcrestart", Post: "/api/settings/timecode-restart", InputId: "tcRestartToggle", Label: "Restart Armed Take on Timecode Restart",
		Readout: readout("RESTART", "FOLLOW"), Hint: "On: the take restarts from its top whenever the code starts rolling or jumps back. Off: it follows the take's own timecode",
		Enabled: tcRestartOn}
}

func handleAPISettingsTCRestart(w http.ResponseWriter, r *http.Request) {
	enabled := r.FormValue("enabled") != ""
	mutex.Lock()
	tcRestartOn = enabled
	settingChanged()
	mutex.Unlock()
	logInfof("Restart armed take on timecode restart %s via remote", map[bool]string{true: "on", false: "off"}[enabled])
	renderFragment(w, switchFragmentTmpl, tcRestartSwitch())
}

// rxLatencySelect is the Audio pane's Receive Latency: the presets, plus the
// current value when a controller set one that is not a preset.
func rxLatencySelect() selectView {
	return settingSelect("rxlatency", "/api/settings/rx-latency", "Receive Latency", "", func() optionsView {
		mutex.Lock()
		defer mutex.Unlock()
		var opts []string
		for _, p := range rxLatencyPresets {
			opts = append(opts, rxLatencyLabel(p))
		}
		idx := rxLatencyPresetIdx(rxLatencyNs)
		if idx < 0 {
			opts = append(opts, rxLatencyLabel(rxLatencyNs)+" (set by a controller)")
			idx = len(opts) - 1
		}
		return optionsView{Options: opts, Idx: idx}
	})
}

func handleAPISettingsRxLatency(w http.ResponseWriter, r *http.Request) {
	handleSelectSetting(w, r, len(rxLatencyPresets), func(i int) {
		setRxLatencyLocked(rxLatencyPresets[i], "via remote")
	}, rxLatencySelect)
}

// tcSettingsFragment is the Timecode pane's contents.
func tcSettingsFragment() template.HTML {
	return frag(selectFragmentTmpl, tcSourceSelect()) +
		frag(selectFragmentTmpl, tcRecordSelect()) +
		frag(switchFragmentTmpl, tcOutputSwitch()) +
		frag(selectFragmentTmpl, tcRateSelect()) +
		frag(switchFragmentTmpl, tcRestartSwitch()) +
		frag(tcPeerFragmentTmpl, currentTCPeerView())
}

func handleAPISettingsTCSource(w http.ResponseWriter, r *http.Request) {
	handleSelectSetting(w, r, len(tcSourceNames), func(i int) {
		tcSourceIdx = i
		if i == tcSourceOff {
			tcDisarmLocked("timecode source off")
		}
		tcSettingsChangedLocked()
		settingChanged()
		logInfof("Timecode source set to %s via remote", tcSourceNames[i])
	}, tcSourceSelect)
}

func handleAPISettingsTCRecord(w http.ResponseWriter, r *http.Request) {
	handleSelectSetting(w, r, len(tcRecordNames), func(i int) {
		tcRecordIdx = i
		tcSettingsChangedLocked()
		settingChanged()
		logInfof("Record timecode set to %s via remote", tcRecordNames[i])
	}, tcRecordSelect)
}

func handleAPISettingsTCRate(w http.ResponseWriter, r *http.Request) {
	handleSelectSetting(w, r, len(tcRates), func(i int) {
		tcRateIdx = i
		tcSettingsChangedLocked()
		settingChanged()
		logInfof("Timecode rate set to %s fps via remote", tcRates[i].Name)
	}, tcRateSelect)
}

func handleAPISettingsTCOutput(w http.ResponseWriter, r *http.Request) {
	enabled := r.FormValue("enabled") != ""
	mutex.Lock()
	tcOutputOn = enabled
	tcSettingsChangedLocked()
	settingChanged()
	mutex.Unlock()
	logInfof("Timecode output %s via remote", map[bool]string{true: "on", false: "off"}[enabled])
	renderFragment(w, switchFragmentTmpl, tcOutputSwitch())
}

func handleAPISettingsTCPeer(w http.ResponseWriter, r *http.Request) {
	peer := strings.TrimSpace(r.FormValue("peer"))
	if peer != "" && !validMTCPeer(peer) {
		v := currentTCPeerView()
		v.Peer, v.Err = peer, "host name or address, optionally :port"
		renderFragment(w, tcPeerFragmentTmpl, v)
		return
	}
	mutex.Lock()
	tcMTCPeer = peer
	tcSettingsChangedLocked()
	settingChanged()
	mutex.Unlock()
	logInfof("MTC peer set to %q via remote", peer)
	renderFragment(w, tcPeerFragmentTmpl, currentTCPeerView())
}

// handleAPITimecodeArm arms (arm=1) or disarms (arm=0) the chase of the
// selected take. JSON for API clients; the dashboard reads the state back
// from the meter push.
func handleAPITimecodeArm(w http.ResponseWriter, r *http.Request) {
	arm := r.FormValue("arm") != "0"
	mutex.Lock()
	var err error
	if arm {
		err = tcArmLocked()
	} else {
		tcDisarmLocked("via remote")
	}
	if err != nil {
		showWebNotice("Chase not armed: " + err.Error())
	}
	mutex.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusConflict)
	}
	json.NewEncoder(w).Encode(currentTimecodeStatus())
}

// tcMeterView is the timecode state the dashboard shows, pushed with the
// meters and served at GET /api/timecode.
type tcMeterView struct {
	Source string `json:"source"` // Off, LTC, MTC
	Locked bool   `json:"locked"`
	In     string `json:"in"`     // "LTC 10:00:00:12" / "LTC --:--:--:--" / "off"
	InLong string `json:"inLong"` // with the rate
	Out    string `json:"out"`    // generator label, "" when off or stopped
	Armed  bool   `json:"armed"`
	Chase  string `json:"chase"` // chase state, "" when not armed
	Take   string `json:"take,omitempty"`
	Peers  string `json:"peers,omitempty"`
}

func currentTimecodeStatus() tcMeterView {
	in, long := tcStatusText()
	v := tcMeterView{In: in, InLong: long, Out: tcOutputText(), Peers: tcPeersText()}
	v.Locked = tcInputNow(time.Now()).locked
	mutex.Lock()
	v.Source = tcSourceNames[tcSourceIdx]
	v.Armed, v.Chase = tcChase.armed, tcChaseTextLocked()
	if tcChase.armed {
		v.Take = filepath.Base(tcChase.file)
	}
	mutex.Unlock()
	return v
}

func handleAPITimecode(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(currentTimecodeStatus())
}

func registerTimecodeRoutes(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("POST /api/settings/timecode-source", auth(handleAPISettingsTCSource))
	mux.HandleFunc("POST /api/settings/timecode-record", auth(handleAPISettingsTCRecord))
	mux.HandleFunc("POST /api/settings/timecode-rate", auth(handleAPISettingsTCRate))
	mux.HandleFunc("POST /api/settings/timecode-output", auth(handleAPISettingsTCOutput))
	mux.HandleFunc("POST /api/settings/timecode-peer", auth(handleAPISettingsTCPeer))
	mux.HandleFunc("POST /api/settings/timecode-restart", auth(handleAPISettingsTCRestart))
	mux.HandleFunc("POST /api/settings/rx-latency", auth(handleAPISettingsRxLatency))
	mux.HandleFunc("POST /api/timecode/arm", auth(handleAPITimecodeArm))
	mux.HandleFunc("GET /api/timecode", auth(handleAPITimecode))
}
