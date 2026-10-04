// The console's worker: a navigation goes to the server, and when the server
// can't be reached the worker answers with the offline page it carries.
// Everything else goes to the server untouched; nothing is cached.

const offline = {{.}};

// A new worker takes over the open console at once.
self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', e => e.waitUntil(self.clients.claim()));

self.addEventListener('fetch', e => {
	if (e.request.mode !== 'navigate') return;
	e.respondWith(fetch(e.request).catch(() => new Response(offline, {
		headers: {'Content-Type': 'text/html; charset=utf-8'},
	})));
});
