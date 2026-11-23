# 调度与计费端到端测试

对一个真实运行的 SmartRouter 实例（SQLite、全新数据库）和本地模拟上游做黑盒检查：
优先级、权重、故障转移与恢复、会话粘性与回切，以及每一步的额度结算是否精确。

```bash
# 1. 模拟上游（四个端口，失败率和延迟由脚本通过 POST /__ctl 设置）
python3 mock_upstream.py 4101 4102 4103 4104 &

# 2. 用全新数据库启动服务
PORT=3300 SQLITE_PATH=./data/sr.db SESSION_SECRET=... CRYPTO_SECRET=... \
  MEMORY_CACHE_ENABLED=true SYNC_FREQUENCY=5 ./new-api &

# 3. 运行
SR_BASE=http://127.0.0.1:3300 SR_DB=./data/sr.db python3 e2e.py
```

脚本会在空库上创建 root 用户（仅测试用密码）、令牌、渠道和价格，只能用于一次性的测试库。
`SR_BASELINE=1` 用于对比本次调度修改之前的版本：跳过新增配置项，其余检查相同。
