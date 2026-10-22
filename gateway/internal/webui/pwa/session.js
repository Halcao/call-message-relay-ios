/* CallRelay PWA — browser session & standalone pairing.
 *
 * Two deliberate pairing paths, both ending at the SAME revocable,
 * sliding-TTL, HttpOnly-cookie session:
 *
 *  1) App-assisted: the owner generates a bind code in the native app
 *     (scope "push" for notifications only, or scope "client" for the full
 *     standalone client) and types it here.
 *  2) Fully standalone (no native app): the owner creates an enrollment key
 *     in the gateway console (shown once as keyId.secret), pastes it here;
 *     the browser generates an Ed25519 device keypair in WebCrypto, enrolls
 *     as its own device, mints a one-shot client bind code for that device,
 *     redeems it — then DISCARDS the device tokens. The only durable
 *     credential is the cookie session; page JS never sees a usable token
 *     (client-scope bind responses omit the bearer), and nothing touches
 *     localStorage.
 *
 * Security notes:
 *  - All requests are same-origin fetches with credentials included; the
 *    gateway enforces SameSite=Strict + full-origin checks server-side.
 *  - Enroll keys and bind codes are owner-provided secrets; they are never
 *    persisted, never placed in URLs (fragments only, never transmitted by
 *    the browser).
 *  - 401 responses mean "session gone": callers re-run pairing.
 */
(function (root) {
  'use strict';

  function api(path, options) {
    options = options || {};
    options.credentials = 'same-origin';
    options.headers = options.headers || {};
    if (options.body !== undefined && !(options.body instanceof Blob) &&
        !(options.body instanceof ArrayBuffer) && !(options.body instanceof Uint8Array)) {
      options.headers['Content-Type'] = 'application/json';
      options.body = JSON.stringify(options.body);
    }
    return fetch(path, options).then(function (resp) {
      return resp.json().catch(function () { return {}; }).then(function (body) {
        if (!resp.ok) {
          var err = new Error(body.message || ('HTTP ' + resp.status));
          err.status = resp.status;
          err.code = body.code;
          throw err;
        }
        return body;
      });
    });
  }

  var Session = {
    api: api,

    /* Current session shape from the cookie, or null when unbound/expired.
     * Safe GET: navigations without Origin must keep working (reload). */
    current: function () {
      return api('/api/v2/device').then(function (body) {
        return body && body.device ? body : null;
      }).catch(function (err) {
        if (err.status === 401 || err.status === 403) { return null; }
        throw err;
      });
    },

    /* Path 1: redeem an owner-generated bind code. Returns the scope. */
    bind: function (code) {
      return api('/api/v2/webpush/bind', {
        method: 'POST',
        body: { code: code },
      }).then(function (body) {
        return body.scope || 'push';
      });
    },

    /* Mint a client-scoped bind code using a short-lived device token
     * (standalone pairing step 3). The token is discarded afterwards. */
    mintClientCode: function (accessToken, deviceID) {
      return api('/api/v2/devices/' + encodeURIComponent(deviceID) + '/webpush/bind-token', {
        method: 'POST',
        headers: { 'Authorization': 'Bearer ' + accessToken },
        body: { scope: 'client' },
      }).then(function (body) { return body.code; });
    },

    /* Ed25519 availability (Safari 17+ / Chromium 137+). */
    ed25519Supported: function () {
      if (!crypto || !crypto.subtle || !crypto.subtle.generateKey) { return false; }
      var probe = { name: 'Ed25519' };
      try {
        return crypto.subtle.generateKey(probe, false, ['sign']).then(function () {
          return true;
        }).catch(function () { return false; });
      } catch (e) {
        return Promise.resolve(false);
      }
    },

    /* Standalone enroll: keyText is the connection key from the console.
     * Returns { deviceId } after a client session cookie is in place.
     * The device name is optional (a default is used). */
    enrollStandalone: function (keyText, deviceName) {
      var cut = keyText.trim().indexOf('.');
      if (cut <= 0) {
        return Promise.reject(new Error('连接密钥格式不正确，请从网关控制台重新复制'));
      }
      var enrollmentKey = keyText.trim();
      var name = (deviceName && deviceName.trim() ? deviceName.trim() : '浏览器客户端').slice(0, 64);
      return Session.ed25519Supported().then(function (ok) {
        if (!ok) { throw new Error('此浏览器不支持免 App 配对，请在 App 中生成浏览器绑定码。'); }
        return crypto.subtle.generateKey({ name: 'Ed25519' }, true, ['sign']);
      }).then(function (keyPair) {
        return crypto.subtle.exportKey('raw', keyPair.publicKey).then(function (rawPublic) {
          var gatewayId = null;
          return api('/api/v2/identity').then(function (identity) {
            gatewayId = identity.gatewayId;
            var message = enrollmentKey.slice(0, enrollmentKey.indexOf('.')) + '\n' +
              enrollmentKey.slice(enrollmentKey.indexOf('.') + 1) + '\n' +
              gatewayId + '\n' + name;
            return crypto.subtle.sign(
              { name: 'Ed25519' },
              keyPair.privateKey,
              new TextEncoder().encode(message));
          }).then(function (signature) {
            return api('/api/v2/enroll', {
              method: 'POST',
              body: {
                enrollmentKey: enrollmentKey,
                deviceName: name,
                devicePublicKey: b64(new Uint8Array(rawPublic)),
                proof: b64(new Uint8Array(signature)),
              },
            });
          });
        });
      }).then(function (pair) {
        // One-shot client code, then redeem and discard the device tokens.
        return Session.mintClientCode(pair.accessToken, pair.deviceId).then(function (code) {
          return Session.bind(code);
        }).then(function (scope) {
          if (scope !== 'client') { throw new Error('配对范围异常，请重试'); }
          return { deviceId: pair.deviceId };
        });
      });
    },
  };

  function b64(bytes) {
    var binary = '';
    for (var i = 0; i < bytes.length; i += 1) { binary += String.fromCharCode(bytes[i]); }
    return btoa(binary);
  }

  root.CallRelaySession = Session;
}(typeof self !== 'undefined' ? self : this));
