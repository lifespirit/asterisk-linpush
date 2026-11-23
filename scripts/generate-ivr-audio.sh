#!/usr/bin/env bash
# Generate the Russian IVR prompts using locally installed eSpeak and FFmpeg.
# Output is 8 kHz, 16-bit mono PCM WAV, readable by Asterisk.
set -euo pipefail
output="${1:-./ivr-ru}"
mkdir -p "$output"
for command in espeak ffmpeg; do
  command -v "$command" >/dev/null || { echo "Missing $command" >&2; exit 1; }
done
generate() {
  local name="$1" phrase="$2" temp
  temp="$(mktemp --suffix=.wav)"
  trap 'rm -f "$temp"' RETURN
  espeak -v ru -s 135 -p 46 -a 160 -w "$temp" "$phrase"
  ffmpeg -nostdin -hide_banner -loglevel error -y -i "$temp" -ar 8000 -ac 1 -sample_fmt s16 -c:a pcm_s16le "$output/$name.wav"
}
generate linpush-menu "Управление пуш уведомлениями. Нажмите один, чтобы узнать состояние. Два, чтобы включить уведомления. Три, чтобы выключить уведомления. Ноль, чтобы выйти."
generate linpush-enabled "Пуш уведомления включены."
generate linpush-disabled "Пуш уведомления выключены."
generate linpush-error "Не удалось изменить настройки. Попробуйте ещё раз."
generate linpush-goodbye "До свидания."
generate linpush-denied "Недостаточно прав для управления уведомлениями."
echo "Created 6 prompts in $output"
