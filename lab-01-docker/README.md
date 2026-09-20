# Лабрадорная работа № 1 — свой Docker

Первоисточник: [условие лабораторной работы](https://github.com/KeladKaal/containerization-and-orchestration/blob/main-rus/lecture-1-docker/lab.md).

Цель работы — последовательно воспроизвести основные механизмы контейнерного
runtime средствами Linux и сравнить результат с запуском через Docker.

## Окружение

- ОС: Linux Mint;
- ядро: Linux 7.0.0-28-generic, x86_64;
- cgroups: cgroup v2;
- язык сервиса: Go 1.25.6.

## Часть 0 — тестовый HTTP-сервис

Вспомогательный `api` сервис реализован на Go. Предоставляет три эндпоинта:

- `GET /health` возвращает `ok`;
- `GET /eat?mb=N` выделяет и удерживает `N` МиБ памяти;
- `GET /burn` запускает бесконечный цикл, нагружающий одно ядро CPU.

Для эксперимента с памятью сервис не только выделяет срез байтов, но и обращается
к каждой странице памяти. Ссылки на выделенные блоки сохраняются, поэтому сборщик
мусора Go не может освободить их до завершения процесса.

## Часть 1 — запуск напрямую

Сервис собран в отдельный бинарник и запущен напрямую, без контейнерного рантайма:

```bash
cd lab-01-docker/api
go build -o /tmp/lab1-api .
/tmp/lab1-api
```

![Запуск API напрямую](docs/screenshots/part-01-api-start.png)

Проверка `GET /health` вернула HTTP 200 и строку `ok`:

```bash
curl -i http://127.0.0.1:8080/health
```

![Проверка health endpoint](docs/screenshots/part-01-health.png)

### Процесс с точки зрения хоста

Имя запущенного бинарного файла — `lab1-api`. Зная его, можно найти PID процесса:

```bash
pgrep -a -x lab1-api
```

Опция `-x` требует точного совпадения имени процесса, а `-a` добавляет к PID
команду запуска. Во время эксперимента команда вывела:

```text
185525 /tmp/lab1-api
```

Чтобы не подставлять найденное число в следующие команды вручную, PID был забинджен
в переменную `pid` и сразу выведен для проверки:

```bash
pid=$(pgrep -n -x lab1-api)
echo "$pid"
```

Опция `-n` выбирает самый новый процесс, если процессов с таким именем несколько.
Конструкция `$(...)` подставляет результат `pgrep` в переменную.

После этого процесс был просмотрен со стороны хоста:

```bash
ps -f -p "$pid"
```

Во время эксперимента процесс имел PID `185525` и был запущен от пользователя
`david`. Это обычный PID хоста, так как отдельный PID namespace еще не создавался.

Команда `ps` без аргументов не показывала `api`, потому что по умолчанию выбирала
процессы, связанные с текущим терминалом. Сервис был запущен в другом терминале.
Поиск через `pgrep` не ограничивался текущим TTY и нашел процесс по имени.

![Поиск PID и просмотр процесса на хосте](docs/screenshots/part-01-host-process.png)

### Исходное состояние cgroup

В cgroup v2 каждый процесс обязательно принадлежит некоторой cgroup. При прямом
запуске `api` попал в systemd scope терминала, однако отдельная группа с лимитами
для лабораторной еще не создавалась.

Текущая cgroup процесса была прочитана из `/proc`:

```bash
cat "/proc/$pid/cgroup"
```

В cgroup v2 строка начинается с `0::`, а после второго двоеточия находится нужный
путь. Чтобы использовать его дальше, путь был сохранен в отдельную переменную:

```bash
cgroup_path=$(cut -d: -f3 "/proc/$pid/cgroup")
echo "$cgroup_path"
```

Значение `memory.max` в найденной cgroup было проверено командой:

```bash
cat "/sys/fs/cgroup${cgroup_path}/memory.max"
```

Команда вернула `max`, то есть отдельный предел памяти на этом уровне отсутствовал.

![Путь cgroup процесса и исходный лимит памяти](docs/screenshots/part-01-process-cgroup-memory.png)

Файла `cpu.max` в scope процесса не оказалось. Чтобы последовательно найти
ближайшего предка с CPU-контроллером, сначала были получены родительские пути:

```bash
terminal_slice=$(dirname "$cgroup_path")
app_slice=$(dirname "$terminal_slice")
echo "$app_slice"
```

Затем были проверены доступные контроллеры `app.slice`, контроллеры, переданные
его дочерним cgroup, и собственная CPU-квота `app.slice`:

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

Файл `cgroup.controllers` показывает, что CPU-контроллер доступен в `app.slice`
для передачи ниже. При этом в `cgroup.subtree_control` перечислены только `memory`
и `pids`: CPU-контроллер не был передан дочерним cgroup. Поэтому в terminal scope,
где находился процесс, файл `cpu.max` отсутствовал.

![Контроллеры и исходная CPU-квота](docs/screenshots/part-01-cgroup-cpu.png)

Значение `max` у `app.slice` означает отсутствие CPU-квоты, а `100000` — период
ее учета в микросекундах. Иерархические и общесистемные ограничения по-прежнему
могут действовать, но собственных лимитов CPU и памяти для `api` на этом этапе нет.

### Вывод

Работающий сервис пока является обычным процессом хоста: он использует PID
namespace и сеть хоста и не имеет заданных в рамках лабораторной ограничений
ресурсов. Успешный `/health` подтверждает функциональность сервиса, но не наличие
изоляции.

## Часть 2 — namespaces

Для процесса были созданы отдельные namespaces шести типов:

| Namespace | Что изолировано |
| --- | --- |
| `pid` | Нумерация и видимость процессов |
| `mnt` | Таблица монтирований и отдельный `/proc` |
| `net` | Интерфейсы, маршруты, сокеты и порты |
| `uts` | Имя хоста |
| `ipc` | System V IPC и POSIX message queues |
| `user` | Отображение UID/GID и связанные с ним права |

Итоговый запуск со всеми namespaces выглядел так:

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

`--fork` запускает `bash` первым процессом нового PID namespace. После установки
hostname команда `exec` заменяет этот же процесс кодом `api`, не меняя его PID,
поэтому именно `api` остаётся PID 1.

![Запуск API со всеми namespaces](docs/screenshots/part-02-all-namespaces-start.png)

### PID и mount namespaces

Снаружи `api` был виден как обычный процесс с рандомным большим PID. Поле `NSpid` в
`/proc/<pid>/status` показало два номера одного процесса:

```text
NSpid:  3352039  1
```

Первый номер действителен в PID namespace хоста, второй — во вложенном namespace.
Это разные имена одной задачи на
разных уровнях видимости.

![PID процесса с точки зрения хоста](docs/screenshots/part-02-pid-host.png)

Новый `/proc` был смонтирован внутри отдельного mount namespace. `procfs` — это
формируемое ядром виртуальное представление, связанное с PID namespace, а не
обычный каталог с сохранёнными на диске данными. Благодаря новому mount команда
`ps` внутри показала только `api` с PID 1, shell и саму команду `ps`.

Для входа использовался `nsenter`. Первая попытка, конечно, завершилась ошибкой
`setgroups failed`, потому что непривилегированное отображение GID установило
`setgroups=deny`. Параметр `--preserve-credentials` запретил `nsenter` повторно
менять группы и позволил войти с уже настроенным отображением UID/GID:

```bash
nsenter \
  --target "$host_pid" \
  --user --mount --pid \
  --preserve-credentials \
  -- bash
```

![PID 1 и список процессов изнутри](docs/screenshots/part-02-pid-inside.png)

### UTS и user namespaces

После создания UTS namespace процессу было присвоено имя хоста `lab1-api`.
Настоящий hostname ноута остался `enigma-Aspire-A715-75G`.

User namespace отобразил UID 0 внутри на UID 1000 снаружи:

```text
внутренний UID 0 → внешний UID 1000
```

Поэтому снаружи процесс принадлежал пользователю `david`, а внутри `id` показывал
`root`. Права root при этом действовали относительно созданных namespaces; для
объектов хоста ядро продолжало учитывать внешний UID 1000.

![Hostname, пользователь и UID mapping снаружи](docs/screenshots/part-02-uts-user-host.png)

![Hostname, root и PID 1 изнутри](docs/screenshots/part-02-uts-user-inside.png)

### IPC namespace

Для проверки на хосте была создана System V message queue:

```bash
host_queue_id=$(ipcmk -Q | awk '{print $NF}')
ipcs -q
```

Изнутри нового IPC namespace хостовая очередь была не видна. Созданная внутри
очередь также получила `msqid=0`, однако это был другой объект ядра в другом
реестре IPC. Хост продолжал видеть только собственную очередь с тем же числовым
идентификатором.

System V IPC-объект не принадлежит процессу, который его создал, и не исчезает
после завершения `ipcmk`. Хостовая очередь была удалена явно:

```bash
ipcrm -q "$host_queue_id"
```

Внутренняя очередь исчезла при уничтожении её IPC namespace.

![Разные очереди в host и IPC namespace](docs/screenshots/part-02-ipc.png)

### Network namespace

После добавления `--net` сервис получил отдельный сетевой стек. Запрос к
`127.0.0.1:8080` с хоста завершился ошибкой: loopback хоста не имеет отношения к
loopback внутри namespace.

![API недоступен через loopback хоста](docs/screenshots/part-02-net-host.png)

Изначально внутри присутствовал только интерфейс `lo` в состоянии `DOWN`, а
таблица маршрутизации была пуста. После включения внутреннего loopback сервис стал
доступен из своего network namespace:

```bash
ip link set lo up
curl -i http://127.0.0.1:8080/health
```

Запрос вернул HTTP 200 и `ok`. Это не создало соединения с хостом или интернетом:
для него потребовались бы `veth`, IP-адреса, маршруты и forwarding/NAT либо сетевой
мост.

![Пустая сеть и успешный запрос через внутренний loopback](docs/screenshots/part-02-net-inside.png)

### Вывод

Namespaces изменили представление процесса о системе, но не создали отдельное
ядро. Хост по-прежнему видел тот же процесс, тогда как изнутри он был PID 1,
root с отдельным hostname, IPC-реестром и сетевым стеком. Ограничений потребления
ресурсов namespaces не добавили — это задача cgroups.

## Часть 3 — cgroup v2

Namespaces ограничивают видимость, но не потребление ресурсов. Для проверки
контроллеров cgroup v2 сервис запускался напрямую: каждый эксперимент изолировал
один механизм, а объединение с namespaces выполняется далее в общем скрипте.

### Ограничение памяти и OOM

Для API была создана cgroup с пределом 64 МиБ, запрещённым swap и групповым OOM:

```bash
sudo mkdir /sys/fs/cgroup/lab1-memory
echo $((64 * 1024 * 1024)) | sudo tee /sys/fs/cgroup/lab1-memory/memory.max
echo 0 | sudo tee /sys/fs/cgroup/lab1-memory/memory.swap.max
echo 1 | sudo tee /sys/fs/cgroup/lab1-memory/memory.oom.group

pid=$(pgrep -n -x lab1-api)
echo "$pid" | sudo tee /sys/fs/cgroup/lab1-memory/cgroup.procs
```

Перед нагрузкой проверялось, что текущий PID действительно находится в
`/lab1-memory`, а счётчики `memory.events` равны нулю. Запрос на выделение и
удержание 80 МиБ превысил лимит:

```bash
curl --max-time 10 --show-error 'http://127.0.0.1:8080/eat?mb=80'
```

Соединение завершилось без ответа, а процесс получил `SIGKILL`:

![API завершён memory cgroup OOM killer](docs/screenshots/part-03-memory-killed.png)

Счётчики после запроса показали `oom=1`, ненулевой `oom_kill` и
`oom_group_kill=1`; `cgroup.procs` опустел. Значение `max=37` означает количество
неудачных начислений памяти сверх `memory.max`, а не число HTTP-запросов.

![Настройка memory cgroup и OOM-счётчики](docs/screenshots/part-03-memory-oom.png)

При первой проверке после OOM API был запущен повторно (тк не сделал скрин), но новый PID не был
перемещен в `lab1-memory`. Поэтому несколько запросов успешно выделили память.
Это показало, что cgroup связывается с экземпляром процесса, а не с именем
бинарника: новый процесс наследует cgroup своего родителя либо должен быть явно
перемещен. Эксперимент был повторен с проверкой `cgroup.procs` и
`/proc/<pid>/cgroup`.

`memory.events` является набором счетчиков memory controller.
Ошибка `curl` доказывает только потерю соединения, а увеличение `oom_kill`
однозначно связывает завершение с OOM внутри этой cgroup. (кстати сам контроллер процесс
не перезапускает, но в Kubernetes завершение обычно обнаруживает kubelet/runtime и применяет
restart policy).

### CPU throttling

Для CPU была задана квота 50 000 мкс на период 100 000 мкс:

```bash
sudo mkdir /sys/fs/cgroup/lab1-cpu
echo '50000 100000' | sudo tee /sys/fs/cgroup/lab1-cpu/cpu.max
pid=$(pgrep -n -x lab1-api)
echo "$pid" | sudo tee /sys/fs/cgroup/lab1-cpu/cgroup.procs
```

Это соответствует половине одного CPU. До нагрузки счетчики throttling были
нулевыми:

![CPU-квота и исходный cpu.stat](docs/screenshots/part-03-cpu-before.png)

Нагрузка запускалась по ручке `/burn`:

```bash
curl http://127.0.0.1:8080/burn
```

Процесс продолжил работать, но после исчерпания 50 мс квоты в очередном периоде
ядро откладывало его выполнение до следующего периода. В `cpu.stat` выросли
`nr_throttled` и `throttled_usec`:

![CPU throttling под нагрузкой](docs/screenshots/part-03-cpu-throttling.png)

Повторная проверка показала дальнейший рост счетчиков: `nr_periods` изменился с
221 до 825, а `nr_throttled` — с 217 до 821. Практически каждый период cgroup
упиралась в квоту.

![Рост счётчиков CPU throttling](docs/screenshots/part-03-cpu-growth.png)

Показание `%CPU` обычной команды `ps` было ниже 50%, потому что это среднее за всё
время жизни процесса, включая простой до запуска `/burn`. Фактическое срабатывание
лимита подтверждают счётчики контроллера, а не единичное показание `ps`.

### Ограничение числа процессов

Для безопасной проверки fork-нагрузки была создана отдельная cgroup с пределом
20 задач:

```bash
sudo mkdir /sys/fs/cgroup/lab1-pids
echo 20 | sudo tee /sys/fs/cgroup/lab1-pids/pids.max
```

В cgroup был перемещён дочерний shell. Все запущенные им процессы автоматически
наследовали ту же cgroup и её лимит:

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

Из дочернего shell была запущена нагрузка:

```bash
stress-ng --fork 100 --timeout 10s --metrics-brief
```

`stress-ng` попытался запустить 100 workers, но cgroup заполнилась ровно до
`pids.current=20`. Счётчик `max` в `pids.events` вырос до сотен тысяч: каждая
такая запись означает отклонённый ядром `fork/clone`.

![Достижение pids.max и отказы fork](docs/screenshots/part-03-pids-limit.png)

Нагрузка остановлена через `Ctrl+C`. Существующие процессы не были убиты, а после
их завершения `pids.current` вернулся к одному; накопленный счётчик отказов
сохранился.

![Завершение stress-ng](docs/screenshots/part-03-pids-stress.png)

![Состояние pids controller после нагрузки](docs/screenshots/part-03-pids-after.png)

### Вывод

| Ресурс | Настройка | Поведение при достижении предела | Доказательство |
| --- | --- | --- | --- |
| Память | `memory.max` | OOM и принудительное завершение | `memory.events:oom_kill` |
| CPU | `cpu.max` | Приостановка до следующего периода | `cpu.stat:nr_throttled` |
| Процессы | `pids.max` | Отказ нового `fork/clone` | `pids.events:max` |

Cgroups ограничили то, что namespaces принципиально не контролируют: объем
памяти, процессорное время и число создаваемых задач.
