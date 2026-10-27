import datetime,json,os,subprocess,sys
from pathlib import Path
Path('tmp').mkdir(exist_ok=True)
p=Path('tmp')/('tests_'+datetime.datetime.now().strftime('%Y-%m-%d_%H-%M-%S')+'.jsonl')
args=['go','test','-json']+sys.argv[1:]
print('running',' '.join(args),flush=True)
with p.open('w') as log:
 proc=subprocess.Popen(args,stdout=subprocess.PIPE,text=True)
 for line in proc.stdout:
  log.write(line);log.flush()
  try: event=json.loads(line)
  except json.JSONDecodeError:
   print(line,end='',flush=True);continue
  if event.get('Action') in ('fail','pass') and (event.get('Action')=='fail' or not event.get('Test')):
   print(event['Action'],event.get('Package'),event.get('Test',''),event.get('Elapsed',''),flush=True)
  if 'WARNING: DATA RACE' in event.get('Output',''): print('DATA RACE',event.get('Package'),event.get('Test'),flush=True)
 code=proc.wait()
print('log',p,flush=True)
sys.exit(code)
