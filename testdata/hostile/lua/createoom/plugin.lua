-- Create runs out of memory, 1 MiB at a time (see the hostile plugin's mib).
function Create()
  local hoard, chunk = {}, string.rep(string.rep("x", 1 << 10), 1 << 10)
  while true do
    hoard[#hoard + 1] = chunk .. #hoard
  end
end
