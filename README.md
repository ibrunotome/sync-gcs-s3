<p align="center">
  <img src="https://user-images.githubusercontent.com/4256471/95026448-c7e4f080-0667-11eb-8635-8b45ff010ff3.png" width="80%">
</p>

<h1 align="center">Sync GCS with S3</h1>

## Google official way

`gsutil -m rsync -r -d gs://bucket s3://bucket`

But we wanna auto sync between them, right?

## Sync huge files using Rclone and Cloud Run

This works better to sync huge files like Cloud SQL backups between GCS and S3. A cloud scheduler will call the cloud.run container which will use rclone to sync the buckets. You can see the original article with this idea and code [here](https://medium.com/@salmaan.rashid/rclone-storage-bucket-sync-using-cloud-scheduler-and-cloud-run-f0ecb8052642).

### Setup

1. Configure Service Accounts Cloud Run and Cloud Scheduler:

```bash
export PROJECT_ID=`gcloud config get-value core/project`
export PROJECT_NUMBER=`gcloud projects describe $PROJECT_ID --format="value(projectNumber)"`
export REGION=us-central1
export RSYNC_SERVER_SERVICE_ACCOUNT=rsync-sa@$PROJECT_ID.iam.gserviceaccount.com
export RSYNC_SRC=gcs-bucket-name
export RSYNC_DEST=s3-bucket-name
export AWS_ACCESS_KEY_ID=your-aws-key
export AWS_SECRET_ACCESS_ID=your-aws-secret
export AWS_REGION=your-region

gcloud iam service-accounts create rsync-sa --display-name "RSYNC Service Account" --project $PROJECT_ID

export SCHEDULER_SERVER_SERVICE_ACCOUNT=rsync-scheduler@$PROJECT_ID.iam.gserviceaccount.com

gcloud iam service-accounts create rsync-scheduler --display-name "RSYNC Scheduler Account" --project $PROJECT_ID
```

2. Configure source and destination GCS Buckets

Configure [Uniform Bucket Access Policy](https://cloud.google.com/storage/docs/uniform-bucket-level-access)

```bash
gsutil iam ch serviceAccount:$RSYNC_SERVER_SERVICE_ACCOUNT:objectViewer gs://$RSYNC_SRC
```

3. Build and deploy Cloud Run image

The `server.go` as an extra secondary check for the audience value that the Cloud Scheduler sends.  This is not a necessary step since Cloud Run checks the audience value by itself automatically (see [Authenticating service-to-service](https://cloud.google.com/run/docs/authenticating/overview)).

This secondary check is left in to accommodate running the service on any other platform.

To deploy, we first need to find out the URL for the Cloud Run instance. 

First build and deploy the cloud run instance (dont' worry about the `AUDIENCE` value below)

```bash
docker build -t gcr.io/$PROJECT_ID/rsync  .

docker push gcr.io/$PROJECT_ID/rsync

gcloud beta run deploy rsync  --image gcr.io/$PROJECT_ID/rsync \
  --set-env-vars AUDIENCE="https://rsync-random-uc.a.run.app" \
  --set-env-vars GS=$RSYNC_SRC \
  --set-env-vars S3=$RSYNC_DEST \
  --set-env-vars AWS_ACCESS_KEY_ID=$AWS_ACCESS_KEY_ID \
  --set-env-vars AWS_SECRET_ACCESS_ID=$AWS_SECRET_ACCESS_ID \
  --set-env-vars AWS_REGION=$AWS_REGION \
  --region $REGION --platform=managed \
  --no-allow-unauthenticated \
  --service-account $RSYNC_SERVER_SERVICE_ACCOUNT
```

Get the URL and redeploy

```bash
export AUDIENCE=`gcloud beta run services describe rsync --platform=managed --region=$REGION --format="value(status.address.url)"`

gcloud beta run deploy rsync --image gcr.io/$PROJECT_ID/rsync \
  --set-env-vars AUDIENCE="$AUDIENCE" \
  --set-env-vars GS=$RSYNC_SRC \
  --set-env-vars S3=$RSYNC_DEST \
  --set-env-vars AWS_ACCESS_KEY_ID=$AWS_ACCESS_KEY_ID \
  --set-env-vars AWS_SECRET_ACCESS_ID=$AWS_SECRET_ACCESS_ID \
  --set-env-vars AWS_REGION=$AWS_REGION \
  --region $REGION --platform=managed \
  --no-allow-unauthenticated \
  --service-account $RSYNC_SERVER_SERVICE_ACCOUNT
```

Configure IAM permissions for the Scheduler to invoke Cloud Run:

```bash
gcloud run services add-iam-policy-binding rsync --region $REGION --platform=managed \
  --member=serviceAccount:$SCHEDULER_SERVER_SERVICE_ACCOUNT \
  --role=roles/run.invoker
```

4. Deploy Cloud Scheduler

First allow Cloud Scheduler to assume its own service accounts OIDC Token:

```bash
envsubst < "bindings.tmpl" > "bindings.json"
```

Where the bindings file will have the root service account for Cloud Scheduler:
- bindings.tmpl:
```yaml
{
  "bindings": [
    {
      "members": [
        "serviceAccount:service-$PROJECT_NUMBER@gcp-sa-cloudscheduler.iam.gserviceaccount.com"
      ],
      "role": "roles/cloudscheduler.serviceAgent"
    }    
  ],
}
```

Assign the IAM permission and schedule the JOB to execute every 5mins:

```bash
gcloud iam service-accounts set-iam-policy $SCHEDULER_SERVER_SERVICE_ACCOUNT  bindings.json  -q

gcloud beta scheduler jobs create http rsync-schedule --schedule "0 1 * * *" \ 
  --http-method=GET \
  --uri=$AUDIENCE \
  --oidc-service-account-email=$SCHEDULER_SERVER_SERVICE_ACCOUNT   \
  --oidc-token-audience=$AUDIENCE
```