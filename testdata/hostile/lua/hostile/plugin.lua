-- The hostile-corpus Lua plugin for failures at call time: Fail(msg) misbehaves as msg says. Its other methods
-- behave like the reference fooer's, so each instance is healthy until provoked.
--
--   error      raises a Lua error
--   oom        allocates until the instance's memory runs out, uncaught
--   oomcaught  the same inside pcall, and fails with what pcall caught
--   recurse    recurses in Lua without end
--   cstack     recurses through C (a metamethod calling itself) without end
--   loop       never returns
--   print      prints to stdout, then raises
--   hugeerror  raises an error message of 1 MiB
--   huge       Foo returns a string too large to copy into the result in the instance's memory
local Hostile = {}
Hostile.__index = Hostile

local function hog()
  local hoard = {}
  while true do
    hoard[#hoard + 1] = string.rep("x", 1 << 20) .. #hoard
  end
end

local function recurse(n)
  return recurse(n + 1) + 1
end

local actions = {
  error = function() error("hostile error") end,
  oom = hog,
  oomcaught = function()
    local ok, err = pcall(hog)
    return nil, "caught: " .. tostring(ok) .. " " .. tostring(err)
  end,
  recurse = function() return nil, "depth " .. recurse(0) end,
  cstack = function()
    local t = setmetatable({}, { __index = function(t, k) return t[k] end })
    return t.x
  end,
  loop = function() while true do end end,
  print = function()
    print("hostile says hello on stdout")
    error("after printing")
  end,
  hugeerror = function() error(string.rep("e", 1 << 20), 0) end,
}

function Hostile:Foo(name)
  if name == "huge" then
    return string.rep("h", 40 << 20)
  end
  return "hello, " .. name
end

function Hostile:Count()
  self.calls = self.calls + 1
  return self.calls
end

function Hostile:Fail(msg)
  local action = actions[msg]
  if action then
    return action()
  end
  return nil, msg
end

function Hostile:Boil(c) return { c >= 100, c * 9 / 5 + 32 } end

function Hostile:Scalars(...) return { ... } end

function Hostile:Spin(n) return n end

function Create()
  return setmetatable({ calls = 0 }, Hostile)
end
