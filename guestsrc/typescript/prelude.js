// The TypeScript guest's prelude, embedded in the artifact by glue.c and evaluated before the plugin: a function
// taking the glue's natives (write, http, utf8Decode, utf8Encode) that installs console, fetch, Headers and
// Response as globals. The natives stay private to this closure.
(function (natives) {
  "use strict";

  const { write, http, utf8Decode, utf8Encode } = natives;

  function show(value) {
    if (typeof value === "string") return value;
    if (value instanceof Error) return value.stack ? `${value.name}: ${value.message}\n${value.stack}` : String(value);
    if (typeof value === "object" && value !== null) {
      try {
        const json = JSON.stringify(value);
        if (json !== undefined) return json;
      } catch {
        // cyclic or throwing: fall back to String
      }
    }
    return String(value);
  }

  function printer(fd) {
    return (...args) => write(fd, args.map(show).join(" ") + "\n");
  }

  const out = printer(1);
  const err = printer(2);
  const console = { log: out, info: out, debug: out, trace: err, warn: err, error: err };

  class Headers {
    #map = new Map(); // lower-case name -> values

    constructor(init) {
      if (init === undefined || init === null) return;
      const entries =
        init instanceof Headers ? init.entries() :
        Array.isArray(init) || typeof init[Symbol.iterator] === "function" ? init :
        Object.entries(init);
      for (const [name, value] of entries) this.append(name, value);
    }

    append(name, value) {
      const key = String(name).toLowerCase();
      const values = this.#map.get(key);
      if (values) values.push(String(value));
      else this.#map.set(key, [String(value)]);
    }

    set(name, value) {
      this.#map.set(String(name).toLowerCase(), [String(value)]);
    }

    get(name) {
      const values = this.#map.get(String(name).toLowerCase());
      return values ? values.join(", ") : null;
    }

    has(name) {
      return this.#map.has(String(name).toLowerCase());
    }

    delete(name) {
      this.#map.delete(String(name).toLowerCase());
    }

    forEach(callback, thisArg) {
      for (const [name, value] of this.entries()) callback.call(thisArg, value, name, this);
    }

    *entries() {
      const names = [...this.#map.keys()].sort();
      for (const name of names) yield [name, this.#map.get(name).join(", ")];
    }

    *keys() {
      for (const [name] of this.entries()) yield name;
    }

    *values() {
      for (const [, value] of this.entries()) yield value;
    }

    [Symbol.iterator]() {
      return this.entries();
    }

    pairs() {
      const pairs = [];
      for (const [name, values] of this.#map) for (const value of values) pairs.push([name, value]);
      return pairs;
    }
  }

  class Response {
    #body;
    #used = false;

    constructor(body, init = {}) {
      this.#body = body === undefined || body === null ? new Uint8Array(0) : toBytes(body);
      this.status = init.status ?? 200;
      this.statusText = init.statusText ?? "";
      this.headers = new Headers(init.headers);
      this.url = init.url ?? "";
      this.redirected = init.redirected ?? false;
      this.ok = this.status >= 200 && this.status <= 299;
    }

    get bodyUsed() {
      return this.#used;
    }

    #consume() {
      if (this.#used) return Promise.reject(new TypeError("Body has already been consumed"));
      this.#used = true;
      return Promise.resolve(this.#body);
    }

    bytes() {
      return this.#consume().then((body) => body.slice());
    }

    arrayBuffer() {
      return this.#consume().then((body) => body.slice().buffer);
    }

    text() {
      return this.#consume().then(utf8Decode);
    }

    json() {
      return this.text().then(JSON.parse);
    }
  }

  function toBytes(body) {
    if (body instanceof Uint8Array) return body;
    if (body instanceof ArrayBuffer) return new Uint8Array(body);
    if (ArrayBuffer.isView(body)) return new Uint8Array(body.buffer, body.byteOffset, body.byteLength);
    return utf8Encode(String(body));
  }

  const scheme = /^[a-zA-Z][a-zA-Z0-9+.-]*:/;

  // resolve resolves a redirect's Location against the URL that answered it.
  function resolve(base, location) {
    if (scheme.test(location)) return location;
    const m = /^([a-zA-Z][a-zA-Z0-9+.-]*:)(\/\/[^/?#]*)?([^?#]*)(\?[^#]*)?/.exec(base);
    if (!m) throw new TypeError(`fetch: cannot resolve ${location} against ${base}`);
    const [, prefix, authority = "", path = ""] = m;
    if (location.startsWith("//")) return prefix + location;
    if (location.startsWith("/")) return prefix + authority + location;
    if (location.startsWith("?")) return prefix + authority + path + location;
    // The base's directory segments (without the empty one before its leading slash), then the location's.
    const segments = path.slice(0, path.lastIndexOf("/")).split("/").slice(1).concat(location.split("/"));
    const kept = [];
    for (const segment of segments) {
      if (segment === "..") kept.pop();
      else if (segment !== ".") kept.push(segment);
    }
    return prefix + authority + "/" + kept.join("/");
  }

  const redirects = new Set([301, 302, 303, 307, 308]);

  // fetch makes the request through the host, which applies the Register's HTTP policy to every hop: a refused or
  // failed request rejects with a TypeError carrying the host's reason. Redirects are followed (up to 20) unless
  // init.redirect is "manual" or "error".
  async function fetch(input, init = {}) {
    let url = typeof input === "string" ? input : input && typeof input.url === "string" ? input.url : String(input);
    let method = String(init.method ?? "GET").toUpperCase();
    const headers = new Headers(init.headers);
    let body = init.body === undefined || init.body === null ? null : toBytes(init.body);
    if (typeof init.body === "string" && !headers.has("content-type")) {
      headers.set("content-type", "text/plain;charset=UTF-8");
    }
    const mode = init.redirect ?? "follow";

    for (let hops = 0; ; hops++) {
      const [status, pairs, bytes] = http(method, url, headers.pairs(), body);
      const response = new Response(bytes, { status, headers: pairs, url, redirected: hops > 0 });
      const location = response.headers.get("location");
      if (mode === "manual" || !redirects.has(status) || location === null) return response;
      if (mode === "error") throw new TypeError(`fetch: redirected to ${location}`);
      if (hops >= 20) throw new TypeError("fetch: too many redirects");
      url = resolve(url, location);
      if (status === 303 || ((status === 301 || status === 302) && method === "POST")) {
        method = "GET";
        body = null;
        headers.delete("content-type");
        headers.delete("content-length");
      }
    }
  }

  const define = (name, value) =>
    Object.defineProperty(globalThis, name, { value, writable: true, configurable: true, enumerable: false });

  define("console", console);
  define("Headers", Headers);
  define("Response", Response);
  define("fetch", fetch);
});
