// The module's page, driven through the manager's WebView bridge.
//
// Every command goes through ctl.sh, so this panel, the Action button and the
// boot service all drive the same process. Command strings are constants and
// no user input ever reaches the shell.

"use strict";

var MODDIR = "/data/adb/modules/monbooru";
var CTL = "sh " + MODDIR + "/ctl.sh";
var ACTIONS = ["start", "stop", "restart"];
var STATUS_EVERY_MS = 3000;
var LOG_EVERY_MS = 5000;
var LOG_TAIL_LINES = 200;

var els = {
	version: document.getElementById("version"),
	banner: document.getElementById("banner"),
	chipRun: document.getElementById("chip-run"),
	runText: document.getElementById("run-text"),
	healthText: document.getElementById("health-text"),
	pidText: document.getElementById("pid-text"),
	url: document.getElementById("url"),
	copy: document.getElementById("copy"),
	refresh: document.getElementById("refresh"),
	start: document.getElementById("start"),
	stop: document.getElementById("stop"),
	restart: document.getElementById("restart"),
	notice: document.getElementById("notice"),
	follow: document.getElementById("follow"),
	clear: document.getElementById("clear"),
	log: document.getElementById("log")
};

var buttons = { start: els.start, stop: els.stop, restart: els.restart };
var busy = false;
var pressed = "";
var statusTimer = 0;
var logTimer = 0;

function bridge() {
	return typeof window.ksu !== "undefined" ? window.ksu : null;
}

/* The manager's bridge takes the callback as a string of JavaScript and runs
 * `expression(code, stdout, stderr)` in this page afterwards. A function
 * cannot cross that boundary: it would be stringified into source the page
 * then fails to parse, and the call would look like it never returned. So
 * every call is named here and the bridge calls back by name. */
var answered = {};
var answerSeq = 0;

window.monbooruBridge = {
	returned: function (name, code, out, err) {
		var settle = answered[name];
		if (!settle) return;
		delete answered[name];
		settle(code, out, err);
	}
};

/* Runs one fixed ctl.sh subcommand and resolves with its stdout. */
function exec(cmd, timeoutMs) {
	return new Promise(function (resolve, reject) {
		var ksu = bridge();
		if (!ksu || typeof ksu.exec !== "function") {
			reject(new Error("no shell bridge"));
			return;
		}

		var name = "call" + (++answerSeq);
		var done = false;
		var timer = window.setTimeout(function () {
			if (done) return;
			done = true;
			delete answered[name];
			reject(new Error("the manager did not answer"));
		}, timeoutMs || 12000);

		answered[name] = function (code, out, err) {
			if (done) return;
			done = true;
			window.clearTimeout(timer);
			if (Number(code) === 0) {
				resolve(out || "");
				return;
			}
			reject(new Error(firstLine(err || out) || "exit " + code));
		};

		try {
			ksu.exec(cmd, "{}", 'window.monbooruBridge.returned.bind(null, "' + name + '")');
		} catch (e) {
			done = true;
			window.clearTimeout(timer);
			delete answered[name];
			reject(e);
		}
	});
}

function firstLine(text) {
	return (text || "").split("\n")[0].trim().slice(0, 200);
}

function say(message) {
	var ksu = bridge();
	if (ksu && typeof ksu.toast === "function") {
		try {
			ksu.toast(message);
			return;
		} catch (e) {
			/* fall through to the notice line */
		}
	}
	showNotice(message);
}

function showNotice(message) {
	els.notice.textContent = message;
	els.notice.hidden = false;
}

function clearNotice() {
	els.notice.textContent = "";
	els.notice.hidden = true;
}

/* ctl.sh status prints key=value lines; nothing else is parsed. */
function parseStatus(text) {
	var out = {};
	(text || "").split("\n").forEach(function (line) {
		var at = line.indexOf("=");
		if (at > 0) out[line.slice(0, at)] = line.slice(at + 1);
	});
	return out;
}

function render(status) {
	var running = status.running === "1";
	var healthy = status.health === "ok";

	els.chipRun.className = "chip" + (running ? (healthy ? " ok" : " down") : "");
	els.runText.textContent = running ? "running" : "stopped";

	var healthChip = els.healthText.parentNode;
	healthChip.className = "chip" + (running && healthy ? " ok" : running ? " down" : "");
	els.healthText.textContent = status.health || "down";

	els.pidText.textContent = status.pid || "\u2014";
	els.url.value = status.url || "";

	els.start.disabled = busy || running;
	els.stop.disabled = busy || !running;
	els.restart.disabled = busy || !running;
}

function refresh() {
	return exec(CTL + " status").then(function (out) {
		render(parseStatus(out));
	}).catch(function (err) {
		render({ running: "0", health: "down" });
		showNotice(err.message);
	});
}

function setBusy(on, action) {
	busy = on;
	if (on && buttons[action]) {
		pressed = action;
		buttons[action].textContent = action + "\u2026";
	} else if (pressed && buttons[pressed]) {
		buttons[pressed].textContent = pressed.charAt(0).toUpperCase() + pressed.slice(1);
		pressed = "";
	}
	if (!on) {
		/* render() restores the enabled set from the next status read. */
		els.start.disabled = els.stop.disabled = els.restart.disabled = false;
	}
}

function act(action) {
	if (busy || ACTIONS.indexOf(action) < 0) return;
	clearNotice();
	setBusy(true, action);
	exec(CTL + " " + action, 90000).then(function () {
		say(action === "stop" ? "stopped" : "started");
	}).catch(function (err) {
		showNotice(err.message);
	}).then(function () {
		setBusy(false, action);
		refresh();
		pullLog();
	});
}

function atBottom(node) {
	return node.scrollTop + node.clientHeight >= node.scrollHeight - 8;
}

function pullLog() {
	return exec(CTL + " log").then(function (out) {
		var pre = els.log;
		var stick = els.follow.checked && atBottom(pre);
		pre.textContent = (out || "").trim() || "(no output yet)";
		if (stick) pre.scrollTop = pre.scrollHeight;
	}).catch(function () {
		/* keep the last view; the status line already reports the failure */
	});
}

function loadVersion() {
	var ksu = bridge();
	try {
		if (ksu && typeof ksu.moduleInfo === "function") {
			var mine = JSON.parse(ksu.moduleInfo()).filter(function (m) {
				return m && m.id === "monbooru";
			})[0];
			if (mine && mine.version) {
				els.version.textContent = mine.version;
				return Promise.resolve();
			}
		}
	} catch (e) {
		/* fall back to the property file */
	}
	return exec("sed -n 's/^version=//p' " + MODDIR + "/module.prop").then(function (out) {
		els.version.textContent = (out || "").trim() || "\u2014";
	}).catch(function () {
		els.version.textContent = "\u2014";
	});
}

function startTimers() {
	stopTimers();
	statusTimer = window.setInterval(refresh, STATUS_EVERY_MS);
	logTimer = window.setInterval(function () {
		if (els.follow.checked) pullLog();
	}, LOG_EVERY_MS);
}

function stopTimers() {
	if (statusTimer) window.clearInterval(statusTimer);
	if (logTimer) window.clearInterval(logTimer);
	statusTimer = 0;
	logTimer = 0;
}

function degrade() {
	els.banner.hidden = false;
	els.banner.textContent = "Open this page from the KernelSU manager: the panel drives the server through its shell bridge.";
	[els.start, els.stop, els.restart, els.refresh, els.clear, els.copy].forEach(function (b) {
		b.disabled = true;
	});
	els.log.textContent = "(panel idle: no manager bridge)";
}

function bind() {
	els.refresh.addEventListener("click", function () {
		refresh();
		pullLog();
	});
	els.start.addEventListener("click", function () { act("start"); });
	els.stop.addEventListener("click", function () { act("stop"); });
	els.restart.addEventListener("click", function () { act("restart"); });

	els.copy.addEventListener("click", function () {
		var url = els.url.value;
		if (!url) return;
		var done = function () { say("copied"); };
		if (navigator.clipboard && navigator.clipboard.writeText) {
			navigator.clipboard.writeText(url).then(done, function () {
				els.url.select();
				say("selected; copy it yourself");
			});
			return;
		}
		els.url.select();
		say("selected; copy it yourself");
	});

	els.clear.addEventListener("click", function () {
		els.log.textContent = "(view cleared; the log file is untouched)";
	});

	els.follow.addEventListener("change", function () {
		if (els.follow.checked) pullLog();
	});

	document.addEventListener("visibilitychange", function () {
		if (document.hidden) {
			stopTimers();
			return;
		}
		refresh();
		if (els.follow.checked) pullLog();
		startTimers();
	});
}

function init() {
	bind();

	var ksu = bridge();
	if (ksu && typeof ksu.enableEdgeToEdge === "function") {
		try {
			ksu.enableEdgeToEdge(true);
		} catch (e) {
			/* older managers do not have it */
		}
	}

	if (!ksu) {
		degrade();
		return;
	}

	loadVersion();
	refresh();
	pullLog();
	startTimers();
}

document.addEventListener("DOMContentLoaded", init);
