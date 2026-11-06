-- The Lua test plugin that validates itself (plg.WithValidation), like testdata/plugins/go/validator: it
-- implements testcontract.Fooer with a Create calling host.Value, and its Validate does what the input's Mode says:
--
--   valid      no issue (nil)
--   warn       a warning and an info issue
--   error      an error issue and a warning
--   unknown    an issue of a severity plg does not know
--   fail       returns nil and a message
--   raise      raises an error
--   panic      allocates until the instance's memory runs out
--   loop       never returns
--   malformed  returns an issue whose severity is not a string
--   host       an info issue "create=<host.Value() in Create> validate=<host.Value() in Validate>"
--   http       an info issue "<status code> <body>" of a GET of the input's URL
--
-- Validate also marks its instance validated: Foo("validated") reports whether this instance was.
local Validator = {}
Validator.__index = Validator

local function hog()
  local hoard, chunk = {}, string.rep(string.rep("x", 1 << 10), 1 << 10)
  while true do
    hoard[#hoard + 1] = chunk .. #hoard
  end
end

local modes = {
  valid = function() return nil end,
  warn = function()
    return {
      { severity = "warning", code = "validator.slow", message = "slow", field = "speed" },
      { severity = "info", code = "validator.note", message = "noted" },
    }
  end,
  error = function()
    return {
      { severity = "error", code = "validator.broken", message = "broken", field = "max" },
      { severity = "warning", code = "validator.slow", message = "slow" },
    }
  end,
  unknown = function()
    return { { severity = "fatal", code = "validator.fatal", message = "fatal" } }
  end,
  fail = function() return nil, "validation broke" end,
  raise = function() error("validation broke", 0) end,
  panic = hog,
  loop = function() while true do end end,
  malformed = function() return { { severity = 3 } } end,
  host = function(self)
    return { { severity = "info", code = "validator.host", message = "create=" .. self.origin .. " validate=" .. host.Value() } }
  end,
  http = function(_, check)
    local resp, err = http.request { url = check.URL }
    if not resp then
      return nil, err
    end
    return { { severity = "info", code = "validator.http", message = resp.status .. " " .. resp.body } }
  end,
}

function Validator:Validate(check)
  self.validated = true
  local mode = modes[check.Mode]
  if not mode then
    return nil, "unknown mode " .. check.Mode
  end
  return mode(self, check)
end

function Validator:Foo(name)
  if name == "origin" then
    return self.origin
  elseif name == "validated" then
    return tostring(self.validated)
  end
  return "hello, " .. name:match("^%s*(.-)%s*$") -- testcontract.Greeting
end

function Validator:Count()
  self.calls = self.calls + 1
  return self.calls
end

function Validator:Fail(msg)
  if msg ~= "" then
    return nil, msg
  end
end

function Validator:Boil(c)
  return { c >= 100, c * 9 / 5 + 32 }
end

function Validator:Scalars(b, i, i8, i16, i32, i64, u, u8, u16, u32, u64, f32, f64, s)
  return { b, i, i8, i16, i32, i64, u, u8, u16, u32, u64, f32, f64, s }
end

function Validator:Spin(n)
  return n
end

-- Create keeps what host.Value returns while Create runs.
function Create()
  return setmetatable({ origin = host.Value(), calls = 0, validated = false }, Validator)
end
