-- A Lua test plugin that runs the Lua code it is given: Foo(code) loads code as a chunk and returns what it
-- returns (tostring of anything but a string), or the error loading it. Fail(code) runs code too and fails with
-- whatever error it raises. The suite uses it to probe the sandbox, require and json from Go.
local Eval = {}

local function run(code)
  local chunk, err = load(code, "=eval")
  if not chunk then
    return nil, err
  end
  return chunk()
end

function Eval:Foo(code)
  local v, err = run(code)
  if v == nil and err ~= nil then
    return nil, err
  end
  if type(v) ~= "string" then
    v = tostring(v)
  end
  return v
end

function Eval:Count() return 0 end

function Eval:Fail(code)
  run(code)
end

function Eval:Boil() return { false, 0 } end

function Eval:Scalars(...) return { ... } end

function Eval:Spin(n) return n end

function Create()
  return Eval
end
