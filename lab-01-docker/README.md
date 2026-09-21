# Лабрадорная работа № 1 - свой Docker

Первоисточник: [условие лабораторной работы](https://github.com/KeladKaal/containerization-and-orchestration/blob/main-rus/lecture-1-docker/lab.md)

Если коротко, цель лабы - разобрать привычный `docker run` на детали и руками
собрать вокруг обычного процесса namespaces, cgroups и ограничения прав

Спойлер: магии внутри Docker не нашлось...

![Никакой магии внутри Docker](docs/images/no-docker-magic.jpg)

...зато нашлось МНОГО деталей ядра Linux

## Окружение

- ОС: Linux Mint
- ядро: Linux 7.0.0-28-generic, x86_64
- cgroups: cgroup v2
- язык сервиса: Go 1.25.6

## Часть 0 - завайбленный подопытный HTTP-сервис

Для эксперимента был собран маленький `api` на Go с тремя эндпоинтами:

- `GET /health` возвращает `ok`
- `GET /eat?mb=N` выделяет и удерживает `N` МиБ памяти
- `GET /burn` запускает бесконечный цикл и нагружает одно ядро CPU

С памятью есть важный нюанс: сервис не просто выделяет срез байтов, но и трогает
каждую страницу (ссылки на блоки остаются в памяти, поэтому сборщик мусора Go не сможет прибрать результаты эксперимента)

## Часть 1 - пока просто процесс

Начинаем с честного baseline: просто собираю бинарь и запускаю прямо на хосте, никаких контейнеров

```bash
cd lab-01-docker/api
go build -o /tmp/lab1-api .
/tmp/lab1-api
```

![Запуск API напрямую](docs/screenshots/part-01-api-start.png)

Сначала чекаем здоровбье:

```bash
curl -i http://127.0.0.1:8080/health
```

![Проверка health endpoint](docs/screenshots/part-01-health.png)

### Процесс с точки зрения хоста

Имя бинаря известно, и по нему можно найти PID нужного процесса:

```bash
pgrep -a -x lab1-api
```

`-x` требует точного совпадения имени, а `-a` заодно показывает команду запуска

```text
185525 /tmp/lab1-api
```

Чтобы дальше не копировать число руками, здесь и далее сохраняю PID в переменную и сразу чекаю ее:

```bash
pid=$(pgrep -n -x lab1-api)
echo "$pid"
```

`-n` выбирает самый новый процесс, если одноименных несколько, а `$(...)`
подставляет результат команды в переменную

Теперь смотрим на процесс глазами хоста:

```bash
ps -f -p "$pid"
```

Во время эксперимента это был PID `185525`, пользователь `david` (я), все максимально
базово, отдельного PID namespace пока нет

Тут я затупил и просто `ps` вызвал, API в выводе не оказалось 😱
А потому что обычный
`ps` показывает процессы текущего терминала, а сервис работал во втором.
А вот `pgrep` ищет по имени и текущим TTY не ограничивается

![Поиск PID и просмотр процесса на хосте](docs/screenshots/part-01-host-process.png)

### Естб ли cgroup?

Да, в cgroup v2 процесс вообще не может быть "нигде", после прямого запуска API
попал в systemd scope терминала, просто отдельные лимиты для него еще не были заданы

Смотрим текущую cgroup через `/proc`:

```bash
cat "/proc/$pid/cgroup"
```

В cgroup v2 строка начинается с `0::`, после второго двоеточия лежит нужный путь.
Сохраняем его:

```bash
cgroup_path=$(cut -d: -f3 "/proc/$pid/cgroup")
echo "$cgroup_path"
```

Сначала чекаем память:

```bash
cat "/sys/fs/cgroup${cgroup_path}/memory.max"
```

Видим `max`, а значит отдельного потолка памяти на этом уровне нет

![Путь cgroup процесса и исходный лимит памяти](docs/screenshots/part-01-process-cgroup-memory.png)

А вот файла `cpu.max` в scope процесса вообще не оказалось, но...

![Путь cgroup процесса и исходный лимит памяти](docs/images/why.jpg)

подозрительно, поэтому иду вверх по иерархии к ближайшему предку с CPU controller:

```bash
terminal_slice=$(dirname "$cgroup_path")
app_slice=$(dirname "$terminal_slice")
echo "$app_slice"
```

Проверяю, какие контроллеры доступны в `app.slice`, какие из них переданы детям и
какая квота стоит на самом `app.slice`:

```bash
cat "/sys/fs/cgroup${app_slice}/cgroup.controllers"
cat "/sys/fs/cgroup${app_slice}/cgroup.subtree_control"
cat "/sys/fs/cgroup${app_slice}/cpu.max"
```

Получен следующий результат:

```text
cpu memory pids
memory pids
max 100000
```

Вот и разгадка: CPU controller доступен в `app.slice`, но в
`cgroup.subtree_control` детям переданы только `memory` и `pids`, поетому в scope
терминала файла `cpu.max` и нет, контроллер туда просто не делегировали

![Контроллеры и исходная CPU-квота](docs/screenshots/part-01-cgroup-cpu.png)

`max` у `app.slice` означает отсутствие CPU quota, а `100000` - период учета в
микросекундах. Ограничения выше по иерархии все еще возможны, но своих лимитов
CPU и памяти у API сейчас нет

### Вывод

Пока это обычный процесс хоста: общие PID namespace и сеть, никаких кастомных лимитов
ресурсов. `/health` говорит что сервису "сомнительно, но ОКЭЙ", но НИКАКОЙ изоляции пока нет, вернее не доказывает

## Часть 2 - так называемые namespaces

Теперь делаем процессу собственное представление о системе, для этого
собираю сразу шесть namespaces:

| Namespace | Что изолировано |
| --- | --- |
| `pid` | Нумерация и видимость процессов |
| `mnt` | Таблица монтирований и отдельный `/proc` |
| `net` | Интерфейсы, маршруты, сокеты и порты |
| `uts` | Имя хоста |
| `ipc` | System V IPC и POSIX message queues |
| `user` | Отображение UID/GID и связанные с ним права |

После отдельных экспериментов итоговый запуск получился таким:

```bash
unshare \
  --user --map-root-user \
  --pid --fork \
  --mount --mount-proc \
  --uts \
  --ipc \
  --net \
  --kill-child \
  bash -c 'hostname lab1-api; exec /tmp/lab1-api'
```

`--fork` запускает `bash` первым процессом нового PID namespace. Затем `exec`
заменяет его кодом API без создания нового процесса, поэтому PID 1 достается
именно `lab1-api`, а не shell

![Запуск API со всеми namespaces](docs/screenshots/part-02-all-namespaces-start.png)

### процесс ОДИН, PIDа - два

Снаружи API выглядит как обычный процесс с каким-то рандомным большим host PID. Но поле `NSpid` в
`/proc/<pid>/status` показывает сразу два номера:

```text
NSpid:  3352039  1
```

Первый действует в PID namespace хоста, второй во вложенном. Это НЕ два процесса,
а два имени одной задачи на разных уровнях видимости

полагаю, что это вот ну очень важно понимать!

![PID процесса с точки зрения хоста](docs/screenshots/part-02-pid-host.png)

Одного PID namespace мало, ему нужен соответствующий `/proc`. `procfs` не обычная
папка на диске, а виртуальное представление, которое ядро как-то собирает на лету.
После нового mount команда `ps` наконец показывает только процессы внутри нашей
изоляции 

Первая попытка войти через `nsenter`, конечно же, не прошла гладко и закончилась
`setgroups failed`. Непривилегированное отображение GID выставило
`setgroups=deny`, а `nsenter` попытался снова поменять группы, добавляю
`--preserve-credentials`, оставляю готовый UID/GID mapping в покое и захожу:

```bash
nsenter \
  --target "$host_pid" \
  --user --mount --pid \
  --preserve-credentials \
  -- bash
```

![PID 1 и список процессов изнутри](docs/screenshots/part-02-pid-inside.png)

### Root внутри, простой david снаружи

UTS namespace получил hostname `lab1-api`, настоящий hostname ноута при этом
остался `enigma-Aspire-A715-75G`

User namespace отобразил UID 0 внутри на UID 1000 снаружи:

```text
внутренний UID 0 → внешний UID 1000
```

Внутри `id` показывает root, снаружи процесс все еще принадлежит `david`. Root полномочия действуют относительно
созданных namespaces, а для объектов хоста ядро видит UID 1000 (взлома ж#№*ы не будет)

![Hostname, пользователь и UID mapping снаружи](docs/screenshots/part-02-uts-user-host.png)

![Hostname, root и PID 1 изнутри](docs/screenshots/part-02-uts-user-inside.png)

### Две очереди с одним `msqid=0`

Чтобы IPC namespace не оставался абстракцией, создаю System V message queue на
хосте:

```bash
host_queue_id=$(ipcmk -Q | awk '{print $NF}')
ipcs -q
```

Изнутри хостовую очередь не видно. Более того, новая очередь внутри тоже получила
`msqid=0`, но это другой объект ядра в другом IPC registry. Одинаковый номер тут
не означает один и тот же объект

Очередь System V IPC живет не "внутри процесса" и не исчезает после завершения
`ipcmk`, поэтому хостовую очередь удаляю явно:

```bash
ipcrm -q "$host_queue_id"
```

Внутренняя очередь исчезла вместе со своим IPC namespace

![Разные очереди в host и IPC namespace](docs/screenshots/part-02-ipc.png)

### Своя сеть (пока без сети)

После `--net` у процесса отдельный сетевой стек. Хостовый запрос к
`127.0.0.1:8080` закономерно падает: loopback хоста и loopback внутри namespace
вообще не родственники

![API недоступен через loopback хоста](docs/screenshots/part-02-net-host.png)

Изначально внутри есть только `lo` в состоянии `DOWN`, маршрутов нет. Поднимаю
внутренний loopback и проверяю API из той же сетевой изоляции:

```bash
ip link set lo up
curl -i http://127.0.0.1:8080/health
```

200 OK?!?, но наружу мы от этого не выбрались. Для связи с хостом понадобятся
`veth`, адреса, маршруты и forwarding/NAT или bridge, этим позже займется Docker

![Пустая сеть и успешный запрос через внутренний loopback](docs/screenshots/part-02-net-inside.png)

### Вывод

Namespaces поменяли то, ЧТО процесс видит, но отдельного ядра не создали. Хост
по-прежнему видит тот же процесс, а изнутри у него PID 1, свой hostname, IPC и
сеть. Сколько ресурсов он может съесть, namespaces вообще не волнует, для этого
нужны cgroups

## Часть 3 - cgroups, теперь ставим лимиты

Namespaces никак не ограничивает прожорливость процесса!
Для начала проверяю каждый controller cgroup v2 отдельно, а потом соберу все
вместе в общем скрипте

### Клетка на 64 МиБ

Создаю cgroup с потолком 64 МиБ, нулевым swap и групповым OOM:

```bash
sudo mkdir /sys/fs/cgroup/lab1-memory
echo $((64 * 1024 * 1024)) | sudo tee /sys/fs/cgroup/lab1-memory/memory.max
echo 0 | sudo tee /sys/fs/cgroup/lab1-memory/memory.swap.max
echo 1 | sudo tee /sys/fs/cgroup/lab1-memory/memory.oom.group

pid=$(pgrep -n -x lab1-api)
echo "$pid" | sudo tee /sys/fs/cgroup/lab1-memory/cgroup.procs
```

Перед нагрузкой отдельно проверяю две вещи: текущий PID действительно лежит в
`/lab1-memory`, а `memory.events` пока по нулям. После этого прошу API удержать
80 МиБ при лимите 64:

```bash
curl --max-time 10 --show-error 'http://127.0.0.1:8080/eat?mb=80'
```

Ответа уже не будет, процесс получает `SIGKILL`

![API завершен memory cgroup OOM killer](docs/screenshots/part-03-memory-killed.png)

После запроса вижу `oom=1`, ненулевой `oom_kill`, `oom_group_kill=1` и пустой
`cgroup.procs`. Значение `max=37` - это количество неудачных начислений памяти
сверх `memory.max`, а не 37 запросов

![Настройка memory cgroup и OOM-счетчики](docs/screenshots/part-03-memory-oom.png)

Тут я сам поймал полезную ошибку. После первого OOM поднял API заново, потому что забыл сделать скрин, но новый PID в `lab1-memory` не перенес. В итоге следующие
запросы спокойно выделяли память, и несколько минут казалось, что лимит внезапно
перестал работать

На самом деле cgroup привязана к экземпляру процесса, а не к имени бинарника.
Новый процесс наследует cgroup родителя или перемещается туда явно. Повторяю
эксперимент уже с проверкой `cgroup.procs` и `/proc/<pid>/cgroup`, теперь все
срабатывает предсказуемо

Еще один важный момент: ошибка `curl` доказывает только потерю соединения.
Настоящее доказательство OOM - рост `oom_kill` в `memory.events`. Сам controller
API не перезапустит, в Kubernetes этим уже занимается kubelet/runtime вместе с
restart policy

### Пол-ядра и никакого суицида

Теперь даю процессу 50 000 мкс CPU на каждые 100 000 мкс периода:

```bash
sudo mkdir /sys/fs/cgroup/lab1-cpu
echo '50000 100000' | sudo tee /sys/fs/cgroup/lab1-cpu/cpu.max
pid=$(pgrep -n -x lab1-api)
echo "$pid" | sudo tee /sys/fs/cgroup/lab1-cpu/cgroup.procs
```

Получается половина одного CPU. До нагрузки throttling по нулям:

![CPU-квота и исходный cpu.stat](docs/screenshots/part-03-cpu-before.png)

включаем `/burn`:

```bash
curl http://127.0.0.1:8080/burn
```

В отличие от memory limit, процесс никто не убивает. После исчерпания 50 мс ядро
откладывает его до следующего ~~семестра~~ периода, а в `cpu.stat` растут `nr_throttled` и
`throttled_usec`

![CPU throttling под нагрузкой](docs/screenshots/part-03-cpu-throttling.png)

При повторной проверке `nr_periods` вырос с 221 до 825, а `nr_throttled` с 217 до
821. Почти каждый период cgroup упирается в квоту, все честно

![Рост счетчиков CPU throttling](docs/screenshots/part-03-cpu-growth.png)

`ps` при этом показывал меньше 50%, потому что усреднял CPU за всю жизнь процесса,
включая простой до `/burn`. Поэтому верим не одному снимку `ps`, а счетчикам
controller

### Запрет на создание задач

Для fork нагрузки создаю отдельную cgroup с потолком 20 задач:

```bash
sudo mkdir /sys/fs/cgroup/lab1-pids
echo 20 | sudo tee /sys/fs/cgroup/lab1-pids/pids.max
```

Перемещаю туда дочерний shell. Все его будущие дети автоматически унаследуют эту
cgroup и ее лимит:

```bash
# Терминал 1
bash
echo $$
```

```bash
# Терминал 2
shell_pid=<PID_ИЗ_ПЕРВОГО_ТЕРМИНАЛА>
echo "$shell_pid" | sudo tee /sys/fs/cgroup/lab1-pids/cgroup.procs
```

![Настройка pids cgroup и дочерний shell](docs/screenshots/part-03-pids-before.png)

Из дочернего shell запускаю нагрузку:

```bash
stress-ng --fork 100 --timeout 10s --metrics-brief
```

`stress-ng` просит 100 workers, но cgroup заполняется ровно до
`pids.current=20`. Счетчик `max` в `pids.events` улетает в сотни тысяч, каждая
такая попытка означает отклоненный ядром `fork/clone`

![Достижение pids.max и отказы fork](docs/screenshots/part-03-pids-limit.png)

Останавливаю нагрузку через `Ctrl+C`. Существующие процессы cgroup не убивает,
она просто запрещает создавать новые. После завершения workers `pids.current`
возвращается к одному, накопленный счетчик отказов остается

![Завершение stress-ng](docs/screenshots/part-03-pids-stress.png)

![Состояние pids controller после нагрузки](docs/screenshots/part-03-pids-after.png)

### Вывод

| Ресурс | Настройка | Поведение при достижении предела | Доказательство |
| --- | --- | --- | --- |
| Память | `memory.max` | OOM и принудительное завершение | `memory.events:oom_kill` |
| CPU | `cpu.max` | Приостановка до следующего периода | `cpu.stat:nr_throttled` |
| Процессы | `pids.max` | Отказ нового `fork/clone` | `pids.events:max` |

Cgroups ограничили то, что namespaces даже не пытаются контролировать: память,
процессорное время и количество задач

p.s. слишком много знаний для разраба

![Ментальная модель Docker до и после первой лекции](docs/images/docker-before-after.png)

## Часть 4 - root еще не суперсила

Стены и лимиты готовы, но остается вопрос: что процессу вообще разрешено просить
у ядра? Здесь нужны два разных механизма, capabilities и seccomp

### UID 0 недостаточно

Сначала создаю user namespace вместе с отдельным UTS namespace:

```bash
unshare --user --map-root-user --uts bash
```

Внутри `id` показывает `uid=0(root)`, а effective set пока содержит capabilities.
`CAP_SYS_ADMIN`, например, разрешает изменить hostname внутри UTS namespace:

```bash
hostname capability-demo
```

![Root с capabilities меняет hostname](docs/screenshots/part-04-capabilities-before.png)

Теперь очищаю capability sets перед запуском нового shell:

```bash
setpriv \
  --bounding-set=-all \
  --inh-caps=-all \
  --ambient-caps=-all \
  --no-new-privs \
  sh -c '
    id
    capsh --print | grep -E "^(Current|Bounding set)"
    hostname should-not-work
  '
```

`id` все еще показывает UID 0, но `Current` и `Bounding set` уже пустые, а
`hostname should-not-work` закономерно получает отказ. Имя остается
`capability-demo`

![UID 0 без capabilities не меняет hostname](docs/screenshots/part-04-capabilities-after.png)

ТАКИМ образом, получается, что UID 0 описывает идентичность, но не дает автоматический пропуск на
привилегированную операцию. Ядро отдельно ищет нужную capability в effective set.
Bounding set ограничивает то, что вообще можно получить после `exec`, а
`no_new_privs` запрещает повысить привилегии через setuid или file capabilities

Но для API суперсилы и не нужны: порт 8080, память и CPU нагрузка работают с
пустым набором capabilities

### Seccomp, запрещаем сам syscall

Capabilities отвечают за классы привилегированных действий, но не задают список
syscalls. Для этого собираю отдельный [`seccomp-profile.json`](seccomp-profile.json):

```json
{
  "defaultAction": "SCMP_ACT_ALLOW",
  "syscalls": [
    {
      "names": ["unshare", "setns"],
      "action": "SCMP_ACT_ERRNO",
      "errnoRet": 1
    }
  ]
}
```

Это учебный denylist, НЕ production allowlist: остальные syscalls разрешены, а
`unshare(2)` и `setns(2)` возвращают `errno=1`, то есть `EPERM`

Сам JSON, конечно, ничего в ядро не загрузит. Мой `setpriv` seccomp filters не
поддерживает, поэтому появился небольшой
[`seccomp-launcher.py`](seccomp-launcher.py): читает профиль, собирает filter через
`libseccomp`, загружает его и через `exec` превращается в целевую программу

Проверка максимально топорная: без фильтра `unshare` работает, с тем же вызовом
через launcher получаю `Operation not permitted`

```bash
unshare --user --map-root-user true \
  && echo "unshare without seccomp: allowed"

python3 seccomp-launcher.py \
  seccomp-profile.json \
  unshare --user --map-root-user true
```

![Отказ syscall unshare под seccomp](docs/screenshots/part-04-seccomp-unshare.png)

Теперь запускаю через тот же launcher сам API:

```bash
python3 seccomp-launcher.py seccomp-profile.json /tmp/lab1-api
```

`/health` продолжает отвечать HTTP 200, а `/proc` подтверждает, что filter пережил
`exec` и висит уже на API:

```text
NoNewPrivs:       1
Seccomp:          2
Seccomp_filters:  1
```

![Работающий API с seccomp-фильтром](docs/screenshots/part-04-seccomp-api.png)

`exec` заменил код launcher кодом API, но нового процесса не создал. PID и
seccomp state сохранились, поэтому API наследует filter и уже не может его
ослабить. `Seccomp: 2` означает filter mode, `NoNewPrivs: 1` запрещает получить
новые привилегии через следующие `exec`

### Вывод

Capabilities и seccomp не конкурируют, а закрывают разные уровни:

| Механизм | Что ограничивает | Результат эксперимента |
| --- | --- | --- |
| Capabilities | Отдельные классы привилегированных операций | UID 0 без нужной capability не смог изменить hostname |
| Seccomp | Вход в конкретные syscalls | `unshare(2)` получил `EPERM`, при этом API продолжил работать |

Даже `CAP_SYS_ADMIN` не перепрыгнет запрещенный seccomp syscall. Filter
срабатывает при входе в системный вызов независимо от UID и capabilities

## Часть 5 - собираю свой Docker

Вся база на базе, пора собирать все в один скрипт
[`mydocker.sh`](mydocker.sh), который одной командой уже запускает API с изоляцией, лимитами и
урезанными правами

### Шлагбаум перед запуском

Сначала скрипт создает одну cgroup `lab1-mydocker` и включает в ней сразу все
проверенные ограничения:

| Контроллер | Значение |
| --- | --- |
| `memory.max` | 64 МиБ (`67108864`) |
| `memory.swap.max` | `0` |
| `memory.oom.group` | `1` |
| `cpu.max` | `50000 100000` (половина CPU) |
| `pids.max` | `20` |

Потом `unshare` собирает user, PID, mount, UTS, IPC и network namespaces. Но тут
есть гонка: API не должен успеть стартовать до попадания под лимиты. Поэтому между
созданием процесса и запуском стоит шлагбаум на FIFO:

```text
unshare создает bash → bash блокируется на read из FIFO
                    → host находит PID bash
                    → host записывает PID в cgroup.procs
                    → host пишет start в FIFO
                    → bash продолжает запуск
```

`read` встроен в Bash, значит во время ожидания не появляется лишний дочерний
процесс вне cgroup. Только после записи host PID в `cgroup.procs` внешний скрипт
отправляет `start` и открывает шлагбаум

Дальше процесс ставит hostname `lab1-api` и включает внутренний loopback. Важно
сделать это ДО сброса capabilities, пока у root в user namespace еще есть нужные
полномочия. После этого начинается цепочка `exec`:

```text
bash PID 1
  → setpriv без capabilities и с no_new_privs
  → seccomp-launcher.py
  → lab1-api PID 1
```

Ни один `exec` не создает новый процесс, поэтому API сохраняет PID 1, namespaces
и cgroup membership. На `Ctrl+C` trap останавливает `unshare`, добивает остатки
через `cgroup.kill`, удаляет cgroup и FIFO. Не daemon, конечно, но за собой убирает

![Запуск API через mydocker.sh](docs/screenshots/part-05-mydocker-start.png)

Снаружи у API обычный большой host PID, внутри он PID 1. `NSpid` показывает оба
номера ОДНОГО процесса, а `/proc/<pid>/cgroup` подтверждает
`/lab1-mydocker`. Хостовый loopback сервис не видит, зато через `nsenter` получаю
HTTP 200:

![PID, cgroup и отдельная сеть mydocker.sh](docs/screenshots/part-05-mydocker-isolation.png)

В финале все capability sets пустые, `NoNewPrivs: 1`, `Seccomp: 2`, а hostname и
`/health` при этом на месте

![Права и работа API в mydocker.sh](docs/screenshots/part-05-mydocker-security.png)

### Теперь тот же API через настоящий Docker

Dockerfile относится уже к следующей части, поэтому пока не забегаю вперед.
Готовый бинарб монтирую read-only в локальный `ubuntu:22.04`. Он требует не
выше `GLIBC_2.34`, образ подходит

Запускаю Docker с теми же лимитами и моделью прав:

```bash
sudo docker run \
  --rm \
  --name lab1-docker \
  --hostname lab1-api \
  --memory 64m \
  --memory-swap 64m \
  --cpus 0.5 \
  --pids-limit 20 \
  --cap-drop ALL \
  --security-opt no-new-privileges=true \
  --security-opt "seccomp=$PWD/seccomp-profile.json" \
  --publish 127.0.0.1:8080:8080 \
  --mount type=bind,src=/tmp/lab1-api,dst=/lab1-api,readonly \
  ubuntu:22.04 \
  /lab1-api
```

`--memory-swap` равен `--memory`, поэтому дополнительного swap нет.
Пользовательский seccomp profile специально заменяет встроенный профиль Docker,
иначе сравнение с `mydocker.sh` было бы нечестным

![Запуск того же API через Docker](docs/screenshots/part-05-docker-run.png)

И вот первая разница видна сразу: `127.0.0.1:8080:8080` делает сервис доступным с
хоста, хотя network namespace отдельный. Внутри API PID 1, снаружи PID `616095`.
Docker с cgroup driver `systemd` кладет контейнер в отдельный scope:

```text
/system.slice/docker-<container-id>.scope
```

Высокоуровневые Docker flags в итоге превращаются в уже знакомые файлы cgroup v2:

```text
memory.max:      67108864
memory.swap.max: 0
cpu.max:         50000 100000
pids.max:        20
```

Изнутри на месте hostname, PID 1, нулевые `CapEff` и `CapBnd`, `NoNewPrivs: 1` и
seccomp filter mode. Никакой второй тайной системы лимитов докер не придумал

![PID, cgroup, лимиты и права Docker-контейнера](docs/screenshots/part-05-docker-verification.png)

### Что совпало, а где мы пока на честном слове

| Область | `mydocker.sh` | Docker |
| --- | --- | --- |
| Namespaces | Создаются напрямую через `unshare` | Настраиваются OCI runtime `runc` |
| Cgroups | Фиксированная cgroup создается и удаляется скриптом | `dockerd` управляет systemd scope и метаданными контейнера |
| Ресурсы | Прямая запись в файлы cgroup v2 | Флаги CLI преобразуются в те же файлы cgroup v2 |
| Права | `setpriv` и отдельный Python launcher | OCI-конфигурация для capabilities, `no_new_privs` и seccomp |
| Сеть | Только отдельный `lo`, связи с хостом нет | `veth`, bridge и правила публикации портов |
| Файловая система | Новая mount table и `/proc`, но корень хоста остается видимым | Отдельный rootfs из слоев образа и управляемые mounts |
| Дополнительная защита | AppArmor и cgroup namespace не настроены | Доступны AppArmor (`docker-default`) и отдельный cgroup namespace |
| Жизненный цикл | Shell, поиск дочернего PID и cleanup через trap | Daemon, имена, inspect, logs, автоматическое удаление через `--rm` |
| PID 1 | API является PID 1 | API также PID 1; init появится только с `--init` |

Низкоуровневые значения совпали, Docker не заменяет namespaces и cgroups какой-то
секретной магией. Он надежно собирает их вместе и добавляет rootfs, сеть, security
policies, метаданные и нормальный lifecycle management

Наш скрипт доказал главный принцип, но до production runtime ему еще очень далеко.
И да, оба варианта по-прежнему используют ОБЩЕЕ ядро хоста
