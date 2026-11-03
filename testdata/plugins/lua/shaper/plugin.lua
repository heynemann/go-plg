-- The Lua test plugin for testcontract.Shaper, like testdata/plugins/go/shaper: every data type crosses it.
-- Structs are tables keyed by json name (Order's embedded Base is the field "Base"), nil collections are nil,
-- nil elements of slices and maps are json.null, and times are {seconds, nanos} in UTC.
local Shaper = {}

function Shaper:Echo(o)
  return o
end

function Shaper:EchoPtr(p)
  return p
end

-- count returns the entries of a table (or -1 for nil), as len() of a Go slice or map.
local function count(t)
  if t == nil then
    return -1
  end
  local n = 0
  for _ in pairs(t) do
    n = n + 1
  end
  return n
end

function Shaper:Inspect(o)
  local nilEntry = false
  for _, line in pairs(o.ByCode or {}) do
    if line == json.null then
      nilEntry = true
    end
  end
  return {
    ID = o.Base.id,
    Zone = "UTC",
    Internal = o.Internal or "",
    Unix = o.When.seconds,
    Nanos = o.When.nanos,
    Lines = count(o.Lines),
    ByCode = count(o.ByCode),
    Raw = o.Raw and #o.Raw or -1,
    Tags = count(o.Tags),
    Notes = count(o.Notes),
    Grid = count(o.Grid),
    Origin = o.Origin ~= nil,
    NilEntry = nilEntry,
  }
end

function Shaper:Bump(o, by)
  for _, line in ipairs(o.Lines or {}) do
    line.qty = line.qty + by
  end
  return o
end

local function grow(name, depth, width)
  local tree = { Name = name }
  if depth == 0 then
    return tree
  end
  tree.Kids = {}
  for i = 0, width - 1 do
    tree.Kids[i + 1] = grow(name .. "." .. i, depth - 1, width)
  end
  return tree
end

function Shaper:Grow(depth, width)
  return grow("root", depth, width)
end

function Shaper:Index(lines)
  if lines == nil then
    return nil
  end
  local index = {}
  for _, line in ipairs(lines) do
    local list = index[line.sku] or {}
    list[#list + 1] = line
    index[line.sku] = list
  end
  return index
end

function Shaper:Reverse(b)
  return b and b:reverse()
end

function Shaper:Later(t, seconds)
  return { seconds = t.seconds + seconds, nanos = t.nanos }
end

function Shaper:Sum(base, more)
  for _, n in ipairs(more or {}) do
    base = base + n
  end
  return base
end

function Create()
  return Shaper
end
