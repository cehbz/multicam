// The console as an installed app: the worker that answers when the server
// isn't running, and the screen kept on while the phone charges.

// The worker is fetched past the browser's HTTP cache at every update check.
if ('serviceWorker' in navigator) navigator.serviceWorker.register('/sw.js', {updateViaCache: 'none'});

// The screen stays on while the battery charges and the page is visible: the
// lock is taken when both hold and released when either ends. The browser
// drops the lock when the page is hidden, and it is taken again when the page
// shows.
if (navigator.getBattery && navigator.wakeLock) navigator.getBattery().then(battery => {
	let held = null; // the lock being taken or held, null when none
	const hold = () => {
		const want = battery.charging && document.visibilityState === 'visible';
		if (want && !held) {
			const taking = held = navigator.wakeLock.request('screen').then(lock => {
				lock.onrelease = () => { if (held === taking) held = null; };
				return lock;
			}, () => { if (held === taking) held = null; });
		} else if (!want && held) {
			held.then(lock => lock?.release());
			held = null;
		}
	};
	battery.addEventListener('chargingchange', hold);
	document.addEventListener('visibilitychange', hold);
	hold();
});
