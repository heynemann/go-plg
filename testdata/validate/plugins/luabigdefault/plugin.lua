-- A rule whose Create keeps a 20 MiB string: over TightRule's 16 MiB, under the 64 MiB Lua default.
local Big = {}
Big.__index = Big

function Big:Apply(n)
  return n + #self.blob
end

function Create()
  return setmetatable({ blob = string.rep("x", 20 * 1024 * 1024) }, Big)
end
