-- A Lua test plugin making HTTP requests with http.request, like testdata/plugins/go/fetcher. Foo takes a command:
--
--   GET <url>             one GET
--   POST <url> <n>        one POST with an n-byte body
--   REPEAT <n> <url>      n GETs in a row
--   HOST <url> <host>     one GET with the Host header overridden
--
-- and returns "<status code>|<X-Reply header>|<body>" of the last response.
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
