-- A rule doubling its number, small enough for TightRule's limits.
local Double = {}
Double.__index = Double

function Double:Apply(n)
  return n * 2
end

function Create()
  return setmetatable({}, Double)
end
