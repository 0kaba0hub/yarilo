# yarilo — deployment topology, sizing та HA

## Architecture model

yarilo — Go-application з **goroutines** для concurrency, не fork-per-user як Dovecot C.
Один процес (наприклад `yarilo-imap`) обслуговує N юзер-сесій через goroutines (~100 KB на сесію).

**Multi-binary, multi-process** (per CLAUDE.md):
- 4 окремих binaries в session-роли: `yarilo-imap`, `yarilo-pop3`, `yarilo-submission`, `yarilo-lmtp`
- 4 окремих binaries в proxy-роли (директор): `yarilo-imap-login`, `yarilo-pop3-login`, `yarilo-submission-login`, `yarilo-lmtp-proxy`
- Кожен — окремий процес з власним адресним простором
- Координація між session-процесами в межах backend deployment — через `yarilo-locks`

Goroutines vs fork:
- 1000 юзерів в одному pod-і ≈ 200-500 MB RAM, не 10 GB
- Нема "fat pod" проблеми
- Горизонтальний scaling — для HA, не для ресурсних лімітів

---

## Компоненти

### director deployment
Маршрутизація юзер-конекшинів до backend-ів через **consistent-hashing ring**.
Містить: 4 proxy процеси (`yarilo-imap-login`, `yarilo-pop3-login`, `yarilo-submission-login`, `yarilo-lmtp-proxy`), 3 director процеси (з monitor sidecars), peer-sync ring.
Тут робиться **TLS terminate + passdb auth + preamble write**.

### backend deployment (один на tag = на NFS shard)
Обробка автентифікованих mail-сесій, читання/запис mail+index даних на NFS.
Містить: 4 session-процеси (`yarilo-imap`, `yarilo-pop3`, `yarilo-submission`, `yarilo-lmtp`) + `yarilo-locks` для координації запису.
**Login-proxies в backend-і не потрібні** — приймає plain TCP від director-а, auth state в preamble.
Userdb lookups — через shared `yarilo-auth`.

### shared services (один deployment на всю інсталяцію)
- `yarilo-auth` — passdb (для director-а) + userdb (для всіх)
- `yarilo-anvil` — connection/session limits (read+write з обох сторін)

### Чому `yarilo-locks` per backend, а не shared
- Кожен backend tag має **свою NFS share** — це окремий data scope
- Locks стосуються тільки файлів цієї share, нема сенсу координувати з іншими tag-ами
- Lower latency (local до backend pod-а)
- Blast radius isolation: падіння locks в tag A не зачіпає tag B
- Нема глобального bottleneck

### Хто пише і читає в `yarilo-anvil`

| Хто пише | Що пише |
|:---|:---|
| director's login-proxies | CONNECT/DISCONNECT events (pre-auth connection tracking, per-IP rate limit) |
| backend's session-процеси | SESSION_START / SESSION_END (post-auth, active mail sessions) |

| Хто читає | Коли і навіщо |
|:---|:---|
| director's login-proxies | перед допуском нового конекшину — enforce per-user/per-IP ліміти на конекти + сесії |

Anvil об'єднує conn-state (з директора) + session-state (з бекенду) — обидва писаних з різних місць, читаних в одному (login proxy).

---

## yarilo-locks — design

**Призначення:** координація запису в mailbox/index файли між 4 session-процесами в backend pod-і
(плюс координація між replicas в межах того ж tag-у — на час failover).

**Чому потрібен:** 4 окремі процеси (`yarilo-imap` / `yarilo-pop3` / `yarilo-submission` / `yarilo-lmtp`) живуть в одному pod-і, кожен зі своїм адресним простором. In-process `sync.Mutex` між ними не працює — це різні процеси.

### Lock model

- **Тільки X (exclusive) замки** для запису
- **Читання — без замків** (storage layer забезпечує консистентний snapshot)
- TTL-based (auto-release при crash клієнта, типово 30с з renew кожні 10с)
- Гранулярність: per-mailbox (`mbox:<user>:<папка>`)

| Операція | Lock |
|:---|:---|
| IMAP SELECT / FETCH / SEARCH / IDLE | без замка |
| LIST / LSUB | без замка |
| IMAP APPEND / EXPUNGE / STORE | X на mailbox |
| LMTP delivery | X на mailbox |
| Rename / Delete mailbox | X на mailbox |
| Sieve script update | X на user-scripts |

### Wire protocol (TAB-delimited TCP via mTLS, port :9104)

```
> VERSION\t1\n
< VERSION\t1\tOK\n

> LOCK\t<ресурс>\t<власник>\t<ttl_ms>\n
< OK\t<id_замка>\n        # отримав
< BUSY\t<поточний_власник>\n  # зайнятий

> UNLOCK\t<id_замка>\n
< OK\n | NOT_FOUND\n

> RENEW\t<id_замка>\t<новий_ttl>\n
< OK\n | EXPIRED\n

> EVENT\t<resource>\t<event_type>\t<payload>\n  # опціональний emit для зовнішніх consumer-ів
```

### Що залишається в session-процесах

- Запис mail data (dbox segments / maildir files)
- Запис index updates (під X замком на mailbox)
- UID assignment (під X замком — атомарне читання-збільшення-запис NEXTUID)
- IDLE notifications publication (через yarilo-locks EVENT channel)

### Deadlock prevention

Конвенція в коді: завжди беремо замки в порядку `idx:<user>` → `mbox:<user>:<папка>` → `deliver:<user>:<папка>`.
Якщо щось зависло — TTL спрацьовує і lock звільняється.

### Storage backend

Redis (per-backend-deployment, або в межах того ж namespace). Key: `lock:<ресурс>`, Value: `<власник>|<acquired_at>`, TTL: 30с.
Атомарне взяття через Lua `SET ... NX EX`.

### HA

- 2 replicas `yarilo-locks` per backend deployment, за ClusterIP Service
- Stateless (state в Redis)
- Local Redis OR shared with other components (anvil тощо)

---

## Helm chart structure

```
helm/yarilo-shared        → auth + anvil + Redis (shared для всіх)
helm/yarilo-director      → director pool + monitor sidecars
helm/yarilo-backend       → backend pool (один release на tag = на NFS shard, з власним locks)
```

### yarilo-shared
- `Deployment yarilo-auth` — replicaCount=2, stateless (userdb в зовнішній SQL/LDAP)
- `Deployment yarilo-anvil` — replicaCount=2, state в Redis
- `Deployment redis` (або external) — state backend для anvil
- ClusterIP Service на кожен

### yarilo-director
- `StatefulSet yarilo-director` — replicaCount=3 (peer-sync ring)
- 2 containers per pod: `yarilo-director` + `yarilo-monitor` (sidecar)
- 4 login-proxy процеси (`yarilo-imap-login`, `yarilo-pop3-login`, `yarilo-submission-login`, `yarilo-lmtp-proxy`) — в окремих контейнерах або в master-supervised процесному дереві
- ClusterIP Service — публічний entry: :993/:995/:587/:24
- Headless Service — для peer-sync DNS

### yarilo-backend (один release на tag, наприклад `yarilo-backend-a`)
**4 окремі StatefulSet-и (один на протокол)** в межах одного Helm release-у — для незалежного scaling:

- `StatefulSet yarilo-backend-<tag>-imap` — replicaCount=N (HPA: conn count)
- `StatefulSet yarilo-backend-<tag>-pop3` — replicaCount=M (HPA: poll rate, типово малий)
- `StatefulSet yarilo-backend-<tag>-submission` — replicaCount=P (HPA: outbound rate)
- `StatefulSet yarilo-backend-<tag>-lmtp` — replicaCount=Q (HPA: delivery queue, scale при burst)
- `Deployment yarilo-locks-<tag>` — replicaCount=2, cross-protocol coordination
- `Deployment redis-<tag>` (або shared Redis) — state backend для locks
- Один **PVC NFS (RWX)** — shared всіма 4 StatefulSet-ами в межах tag-у
- 4 Headless Services — по одному на StatefulSet, для sticky routing від director-а

**Чому 4 окремих StatefulSet, а не 1 з 4 контейнерами в pod-і:**
- Process isolation — crash imap не зачіпає lmtp
- Independent scaling — POP3 типово 1 pod, lmtp при mass-delivery 10+
- Right-sized resources — кожен StatefulSet з власними CPU/RAM limits
- HPA per protocol з різних метрик

**Trade-off:** Cross-protocol writes (наприклад LMTP delivery → IMAP STORE на той же mailbox) йдуть через `yarilo-locks` (cross-pod). Locks стає **critical path** для всіх writes.

### Director routing — 4 окремі рінги

Director підтримує 4 незалежних ring-и (один на протокол):
- `ring_imap`: `MD5(user) → yarilo-backend-<tag>-imap-N`
- `ring_pop3`: `MD5(user) → yarilo-backend-<tag>-pop3-M`
- `ring_submission`: ...
- `ring_lmtp`: ...

N, M, P, Q можуть бути різними (різні StatefulSet sizes). Для одного user-а IMAP і LMTP можуть потрапити на різні pod-и — координація через `yarilo-locks`.

---

## Sizing per backend pod

| Workload | RAM | CPU |
|:---|:---|:---|
| 5k idle (mostly IMAP IDLE) | ~300-500 MB | ~0.1 cores |
| 2k active | ~500-800 MB | ~0.3-0.5 cores |
| Burst (FETCH great mail × 100 юзерів) | до ~2 GB peak | до 1-2 cores peak |

**Recommended Helm values:**
```yaml
resources:
  requests:
    cpu: 250m
    memory: 512Mi
  limits:
    cpu: 2000m
    memory: 2Gi
```

**Bottlenecks (по черзі):**
1. **Open FDs** — `ulimit -n 65535` обов'язково (default 1024 мало). ~10 FD per session.
2. **NFS IOPS** — найбільш ймовірний bottleneck. SSD NFS + `cachefilesd`/`fscache` допомагають.
3. **yarilo-locks RTT** — кожен write робить LOCK/UNLOCK. Локальний Redis → ~1-2ms на пару.
4. **NFS bandwidth** — FETCH attachment-ів. 1 Gbps = 125 MB/s.
5. **RAM при burst** — buffer на читання великих mail-ів.

---

## Sizing per backend tag (4 StatefulSets + один NFS share + локальний locks)

| Параметр | Типове значення | Min/Max |
|:---|:---|:---|
| yarilo-imap replicaCount | 3-5 | 1 / scale-out |
| yarilo-pop3 replicaCount | 1-2 | 1 / scale-out (POP3 рідко інтенсивний) |
| yarilo-submission replicaCount | 2-3 | 1 / scale-out |
| yarilo-lmtp replicaCount | 3-5 baseline | 1 / 10+ при mass-delivery burst |
| locks replicaCount | **2** | stateless, ClusterIP |
| Users per pod | **3-5k** mostly idle | Goroutines дешеві, але FD/RAM ліміти |
| **Total users per tag** | **10-20k** | NFS server обмежує |
| NFS share size | **5-10 TB** | 10k юзерів × ~500 MB середній quota |
| NFS sustained IOPS | **10-30k** | Index/mailbox операції |
| NFS bandwidth | **1-10 Gbps** | Attachment transfer |

---

## Коли робити новий tag

| Trigger | Поріг |
|:---|:---|
| NFS storage used | > 70-75% |
| NFS sustained IOPS | > 70% capacity |
| Users per tag | > 15-20k |
| p99 latency IMAP simple ops | > 200ms |

Один Helm release `yarilo-backend` per tag: `yarilo-backend-a`, `yarilo-backend-b`, ...
Кожен зі своїм NFS PV і власним `yarilo-locks` сервісом.

**Приклади масштабу:**
- 10k юзерів → 1 tag, 3 replicas, 5 TB NFS
- 50k юзерів → 3-4 tags, кожен 3 replicas × 5 TB
- 200k+ → 10-15 tags, починаємо думати про non-NFS storage

---

## Director routing & stickiness

**Ring:** `MD5(username) → backend pod` (в межах одного tag).
**Tag assignment:** окрема мапа user → tag (admin-defined або hash-based shard).

1. Клієнт → director's login-proxy (TLS terminate)
2. Director: passdb через `yarilo-auth`, userdb теж
3. Director: визначає user's tag, ring мапить → pod
4. Director конектиться напряму до pod-а за stable DNS (headless Service)
5. Передає auth state в preamble, проксує plain TCP

`userDir` в директорі — in-memory cache активних user → pod mapping-ів. Sync між director-ами через peer protocol.

---

## HA strategy

| Layer | HA approach |
|:---|:---|
| Director | replicaCount=3, peer-sync, monitor sidecar |
| Backend per tag | replicaCount=3-5, shared NFS RWX, ring rebalance |
| yarilo-locks (per tag) | replicaCount=2, state в Redis |
| yarilo-auth | replicaCount=2 stateless |
| yarilo-anvil | replicaCount=2, state в Redis |
| Redis | external HA (Sentinel/Cluster) або managed |
| NFS server | окрема HA задача (Pacemaker+DRBD, або managed NFS типу AWS EFS) |

**Backend failover sequence:**
1. `yarilo-monitor` sidecar виявляє dead pod (~5-10с)
2. Director виключає з ring
3. Locks на dead pod-і протухнуть через TTL (30с) на `yarilo-locks-<tag>`
4. Ring rehash → юзери на сусідні replicas в тому ж tag-і
5. K8s scheduler піднімає новий pod (~30с)
6. Новий pod mount-ить ту ж NFS, починає приймати конекти

---

## Stickiness rationale

Юзер X завжди обслуговується одним backend pod-ом (в межах того ж tag-у). Причини:
- Менше cross-pod lock contention в `yarilo-locks`
- Index cache locality
- Уникнення IDLE notification дубляжу
- Швидше підняття сесії (cache prewarmed)

Stickiness ≠ data partitioning. Дані шарені на NFS, sticky routing — оптимізація.

---

## Чому event-loop (goroutines), а не fork

yarilo на Go. Go runtime + `fork()` = undefined behavior — заборонено в CLAUDE.md.
- `exec.Command` — для запуску дочірніх процесів при старті pod-а
- **goroutines per user** в межах процесу — одна горутина на юзер-сесію

Принципово відрізняється від Dovecot C-моделі (fork per user → процес per user → ~10 MB per session).
yarilo тримає 1000+ юзерів в одному процесі без перевитрати ресурсів.
