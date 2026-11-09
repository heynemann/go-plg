-- Written against v1.2.0 but pinned to v1.3.0, whose Provider adds Size.
local P = {}
P.__index = P

function P:IsMember(user) return false end

function P:List(cursor) return { users = {}, next = "" } end

function Create() return setmetatable({}, P) end
