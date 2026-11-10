-- rexq for Rex: the open-questions tracker as a floating panel that
-- follows you from tab to tab. `rexq install` puts this file next to Rex's
-- init.lua and adds the line that loads it:
--
--   require("rexq").setup{ key = "shift+cmd+i" }
--
-- The key shows the panel floating over the tab you're in. If it is showing
-- in another tab, it moves here. Pressing the key again hides it. Hiding
-- detaches the block rather than closing it, so the tracker keeps running and
-- you come back to the same place. Each Rex session has its own tracker,
-- because a block can't move between sessions.
--
-- setup takes key (false for no key; the action "rexq_toggle" is always
-- in the command palette), bin (the rexq binary, when it isn't the one that
-- installed this file) and bounds (the panel's {x, y, w, h} as fractions of
-- the window).

-- rexq install writes this line.
local BIN = "rexq"

local M = {
  bin = BIN,
  bounds = { 0.06, 0.05, 0.88, 0.9 },
}

local SHELL = "com.superlogical.terminal.shell"
local LABEL = "Agent questions"

-- The tracker block in each session.
local blocks = {}

local function must(res, err)
  if err ~= nil then error(err, 0) end
  return res
end

-- Returns our block's listing, or nil when it has exited.
local function current(sid)
  local id = blocks[sid]
  local list = must(rex.session.list_blocks{ session_id = sid })
  for _, b in ipairs(list.blocks or {}) do
    -- Match by label too, so a config reload doesn't orphan a running tracker.
    if b.block_id == id or (id == nil and b.label == LABEL) then
      blocks[sid] = b.block_id
      return b
    end
  end
  blocks[sid] = nil
end

-- A block in the active window's tiled layout, to place the tracker beside
-- before floating it.
local function anchor(view, skip)
  for _, w in ipairs(view.windows or {}) do
    if w.window_id == view.active_window_id then
      for _, layer in ipairs(w.layers or {}) do
        if layer.kind == "tiled" then
          for _, b in ipairs(layer.blocks or {}) do
            if b.block_id ~= skip then return b.block_id end
          end
        end
      end
    end
  end
end

local function toggle(ctx)
  local sid = ctx.session_id
  if not sid then error("no session to show the panel in", 0) end
  local view = must(rex.session.view{ session_id = sid })

  local b = current(sid)
  if b and b.placement == "placed" and b.window_id == view.active_window_id then
    must(rex.session.detach_block{ session_id = sid, block_id = b.block_id })
    return
  end

  if not b then
    local res = must(rex.session.new_block{
      session_id = sid,
      flavor = SHELL,
      label = LABEL,
      -- --layer: q hides this block instead of quitting the panel.
      options = { command = { M.bin, "panel", "--layer" } },
    })
    blocks[sid] = res.block_id
    b = { block_id = res.block_id }
  end

  local a = anchor(view, b.block_id)
  if not a then error("this tab has no pane to float the panel over", 0) end
  must(rex.session.move_block{ session_id = sid, block_id = b.block_id, anchor_block_id = a })
  must(rex.session.float_block{ session_id = sid, block_id = b.block_id, bounds = M.bounds })
  must(rex.session.focus_block{ session_id = sid, block_id = b.block_id })
end

function M.setup(opts)
  opts = opts or {}
  for k, v in pairs(opts) do
    if k ~= "key" then M[k] = v end
  end
  rex.action{
    name = "rexq_toggle",
    title = "Toggle Agent Questions",
    description = "Show or hide the tracker of questions your agents left for you, in the current tab.",
    keywords = "inbox questions agents snooze claude",
    run = function(ctx) toggle(ctx) end,
  }
  local key = opts.key
  if key == nil then key = "shift+cmd+i" end
  if key then rex.bind(key, "rexq_toggle") end
end

return M
