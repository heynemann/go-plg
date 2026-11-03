-- No Create at all: a local function is not the global the guest calls.
local function Create()
  return {}
end
return Create
