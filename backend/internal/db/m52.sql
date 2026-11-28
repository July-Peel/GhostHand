DELETE FROM chat_messages
WHERE task_id IN (SELECT id FROM tasks WHERE source = 'mcp');
