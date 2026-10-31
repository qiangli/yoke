---
id: cbc93ba6aecc
kind: feature
title: S546.2 AWS EC2 GPU worker adapter in yoke/pkg/provider/aws with budget and GPU guard preflight
seq: 2988
status: todo
priority: p0
labels:
    - cloud
    - aws
    - gpu
created: 2026-10-30T23:46:49.607077Z
sprint: 546
sprint_id: ae111a75-889f-50a4-a423-de1bab0fbca6
sprint_title: On-demand cloud GPU workers for large/custom Ollama models via Cloudbox and Outpost (AWS EC2 first)
---

Extend stdlib SigV4 yoke/pkg/provider/aws to support EC2 GPU worker operations (OpPlan, OpCreate, OpInspect, OpList, OpStart, OpStop, OpDestroy) in us-west-2 under the dhnt-automation-ec2-gpu IAM policy (allowlisted g6.xlarge, g6.2xlarge, g6e.xlarge, g5.xlarge; dhnt:managed=true tag at launch; <=200 GB gp3 volume; egress-only default security group since Outpost dials out; S3-staged presigned user-data bootstrap; dhnt-monthly-cap and dhnt-gpu-daily budget preflight). Include fakeec2 unit/conformance tests with zero cloud spend.

Review 2026-10-30 (operator session, satellite integration): IAM facts from the APPLIED guard (managed policy dhnt-automation-ec2-gpu, us-west-2): RunInstances only for g6.xlarge/g6.2xlarge/g6e.xlarge/g5.xlarge with aws:RequestTag dhnt:managed=true (tag BOTH instance and volume in TagSpecifications; CreateTags only during RunInstances), volumes <=200 GB, Stop/Start/Reboot/Terminate only on dhnt:managed=true; NO ModifyInstanceAttribute (changing type on a stopped worker = new worker), NO CreateVolume/AttachVolume, NO security-group writes (default SG egress only), NO spot SLR; servicequotas:GetServiceQuota is granted for a quota preflight (L-DB2E81BA was 0, 8 vCPU requested 2026-10-30, PENDING). Budget preflight must change for GPU: the Lightsail preflight compares a MONTHLY bundle price to a $24/mo per-instance ceiling, which every GPU type fails; add an hourly model (rate x session hours, plus EBS monthly) checked against dhnt-monthly-cap ($400) remaining, and print the hourly price in the plan. Launch with InstanceInitiatedShutdownBehavior=stop so a guest poweroff stops (never terminates) the worker. Handle InsufficientInstanceCapacity on StartInstances (the volume is AZ-bound: report it, do not silently recreate elsewhere). First boot reuses the S522 S3-staged presigned user-data (bucket dhnt-userdata-<acct>-<region>). Cost facts for the plan: g6.xlarge ~$0.805/h, g5.xlarge ~$1.006/h, g6e.xlarge ~$1.861/h; gp3 $0.08/GB-mo plus provisioned throughput above 125 MB/s billed even while Stopped (~$0.04 per MB/s-mo: 500 MB/s ~ +$15/mo, 1,000 ~ +$35/mo); auto-assigned public IPv4 $0.005/h only while running. Activate credits (Founders + Inception) cover EC2, EBS, data transfer and S3 (checked 2026-10-30). Independent of Sprint 530 (yoke only).
