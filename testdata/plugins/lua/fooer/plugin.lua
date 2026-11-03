-- The reference Lua test plugin: it implements testcontract.Fooer like testdata/plugins/go/fooer, across several
-- files loaded with require (a module, and a module in a subdirectory).
local greeting = require("greeting")
local units = require("lib.units")

-- upper upper-cases ASCII and the Latin-1 letters (U+00E0 to U+00FE but U+00F7), like strings.ToUpper there.
local function upper(s)
  return (s:gsub(utf8.charpattern, function(c)
    local code = utf8.codepoint(c)
    if code >= 0xE0 and code <= 0xFE and code ~= 0xF7 then
      return utf8.char(code - 0x20)
    end
    return c:upper()
  end))
end

local Fooer = {}
Fooer.__index = Fooer

function Fooer:Foo(name)
  return greeting(name)
end

function Fooer:Count()
  self.calls = self.calls + 1
  return self.calls
end

function Fooer:Fail(msg)
  if msg ~= "" then
    return nil, msg
  end
end

function Fooer:Boil(c)
  return { c >= 100, units.fahrenheit(c) }
end

function Fooer:Scalars(b, i, i8, i16, i32, i64, u, u8, u16, u32, u64, f32, f64, s)
  return { not b, i + 1, i8 + 1, i16 + 1, i32 + 1, i64 + 1, u + 1, u8 + 1, u16 + 1, u32 + 1, u64 + 1, f32 * 2,
    f64 * 2, upper(s) }
end

function Fooer:Spin(n)
  local count = 0
  while count < n do
    count = count + 1
  end
  return count
end

function Create()
  return setmetatable({ calls = 0 }, Fooer)
end
