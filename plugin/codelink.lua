-- codelink: keep this nvim instance discoverable by the codelink daemon.
--
-- All the logic lives in the codelink module, required lazily inside the
-- callbacks so startup stays fast and a broken module cannot break startup.
--
-- The guard matters here more than in a typical plugin file: this one owns a
-- socket and a registry file, and registering twice would leak both.
if vim.g.loaded_codelink then
    return
end
vim.g.loaded_codelink = true

local group = vim.api.nvim_create_augroup('codelink', {})

vim.api.nvim_create_autocmd('VimEnter', {
    group = group,
    callback = function()
        require('codelink').register()
    end,
})

vim.api.nvim_create_autocmd({ 'DirChanged', 'FocusGained', 'VimResume' }, {
    group = group,
    callback = function()
        require('codelink').touch()
    end,
})

vim.api.nvim_create_autocmd('VimLeavePre', {
    group = group,
    callback = function()
        require('codelink').unregister()
    end,
})

vim.api.nvim_create_user_command('CodelinkStatus', function()
    local codelink = require('codelink')
    local path = codelink.registry_path()
    local entry = codelink.entry()
    local lines = { 'codelink registry: ' .. path, 'on disk: ' .. (vim.uv.fs_stat(path) and 'yes' or 'no') }
    if entry then
        table.insert(lines, vim.inspect(entry))
    else
        table.insert(lines, 'not registered')
    end
    vim.notify(table.concat(lines, '\n'), vim.log.levels.INFO)
end, { desc = 'Show the codelink registry path and this instance entry' })
