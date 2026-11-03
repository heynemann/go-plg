-- Create runs out of memory.
function Create()
  local hoard = {}
  while true do
    hoard[#hoard + 1] = string.rep("x", 1 << 20) .. #hoard
  end
end
