-- A Lua test plugin making HTTP requests with http.request, like testdata/plugins/go/fetcher. Foo takes a command:
--
--   GET <url>             one GET
--   POST <url> <n>        one POST with an n-byte body
--   REPEAT <n> <url>      n GETs in a row
--   HOST <url> <host>     one GET with the Host header overridden
--   SORTGET <url>         one GET from inside table.sort's comparator, where the call cannot yield
--   FLOOD <n> <size> <url> n POSTs of one size-byte body, one after the other
--   LOOP -                loops forever
--   HOG -                 allocates until it runs out of memory
--   YIELD -               yields from the method itself
--
-- and returns "<status code>|<X-Reply header>|<body>" of the last response; FLOOD returns "<responses>|<the
-- distinct sorted reasons of the failed requests, joined by ;>".
local function get(url, hostname)
  local headers = { ["X-Test"] = "fetcher" }
  if hostname then
    headers.Host = hostname
  end
  return http.request { url = url, headers = headers }
end

local function describe(resp, err)
  if not resp then
    return nil, err
  end
  return resp.status .. "|" .. (resp.headers["X-Reply"] or "") .. "|" .. resp.body
end

local Fetcher = {}

function Fetcher:Foo(cmd)
  local args = {}
  for word in cmd:gmatch("%S+") do
    args[#args + 1] = word
  end
  if #args < 2 then
    return nil, "bad command"
  end
  local verb = args[1]
  if verb == "GET" then
    return describe(get(args[2]))
  elseif verb == "HOST" then
    return describe(get(args[2], args[3]))
  elseif verb == "POST" then
    return describe(http.request {
      method = "POST",
      url = args[2],
      headers = { ["Content-Type"] = "text/plain" },
      body = string.rep("x", tonumber(args[3])),
    })
  elseif verb == "REPEAT" then
    local out
    for _ = 1, tonumber(args[2]) do
      local got, err = describe(get(args[3]))
      if not got then
        return nil, err
      end
      out = got
    end
    return out
  elseif verb == "SORTGET" then
    local resp, err
    local pair = { 2, 1 }
    table.sort(pair, function(a, b)
      if resp == nil and err == nil then
        resp, err = get(args[2])
      end
      return a < b
    end)
    return describe(resp, err)
  elseif verb == "FLOOD" then
    local body = string.rep("x", tonumber(args[3]))
    local ok, seen, reasons = 0, {}, {}
    for _ = 1, tonumber(args[2]) do
      local resp, err = http.request { method = "POST", url = args[4], body = body }
      if resp then
        ok = ok + 1
      elseif not seen[err] then
        seen[err] = true
        reasons[#reasons + 1] = err
      end
    end
    table.sort(reasons)
    return ok .. "|" .. table.concat(reasons, ";")
  elseif verb == "LOOP" then
    while true do
    end
  elseif verb == "HOG" then
    local kept = {}
    while true do
      kept[#kept + 1] = string.rep("x", 1 << 20) .. #kept
    end
  elseif verb == "YIELD" then
    coroutine.yield()
  end
  return nil, "unknown command " .. verb
end

function Fetcher:Count() return 0 end

function Fetcher:Fail() end

function Fetcher:Boil() return { false, 0 } end

function Fetcher:Scalars(...) return { ... } end

function Fetcher:Spin() return 0 end

function Create()
  return Fetcher
end
