/* CallRelay gateway PWA service worker.
 *
 * Push payloads use the Declarative Web Push shape
 * ({"web_push":8030,"notification":{...}}): WebKit (iOS 18.4+) displays the
 * notification and navigates to `navigate` without running this file, and
 * older browsers hand the identical bytes to this push event. One payload
 * therefore serves both paths. The navigate URL carries the handoff token in
 * the URL fragment, which never reaches any access log.
 */
self.addEventListener('install', (event) => {
  event.waitUntil(self.skipWaiting());
});

self.addEventListener('activate', (event) => {
  event.waitUntil(self.clients.claim());
});

self.addEventListener('push', (event) => {
  let envelope = {};
  try {
    envelope = event.data ? event.data.json() : {};
  } catch (e) {
    envelope = {};
  }
  const note = envelope.notification || envelope;
  const title = note.title || 'CallRelay';
  const options = {
    body: note.body || '',
    tag: note.tag || 'callrelay',
    renotify: Boolean(note.renotify),
    silent: Boolean(note.silent),
    icon: '/pwa/icon-192.png',
    badge: '/pwa/icon-192.png',
    data: { navigate: note.navigate || '/pwa/' },
  };
  event.waitUntil(self.registration.showNotification(title, options));
});

self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  const target = (event.notification.data && event.notification.data.navigate) || '/pwa/';
  event.waitUntil(
    self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then((clients) => {
      for (const client of clients) {
        if (client.url.indexOf(self.registration.scope) !== 0) continue;
        const hashIndex = target.indexOf('#');
        const hash = hashIndex >= 0 ? target.slice(hashIndex) : '';
        const clientPath = new URL(client.url).pathname;
        const targetPath = new URL(target, self.registration.scope).pathname;
        if (clientPath === targetPath) {
          // Same page already open: a fragment change does not reload it, so
          // tell the page to restart its flow for the new call and focus it.
          // NEVER also navigate() here — that reload would redeem the
          // single-use handoff token a second time and the tap would die.
          if (hash) client.postMessage({ type: 'callrelay-navigate', hash });
          return client.focus();
        }
        // Different page: navigate (full load) and focus; no message needed.
        return client.navigate(target).then((focused) => {
          if (focused) return focused.focus();
          client.focus();
          return undefined;
        });
      }
      return self.clients.openWindow(target);
    })
  );
});
